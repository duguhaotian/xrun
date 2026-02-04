// Package cloudhypervisor provides a Cloud-Hypervisor VMM driver implementation.
package cloudhypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/microvm/sandbox/pkg/log"
	"github.com/microvm/sandbox/pkg/vmm"
)

const (
	defaultBinaryName = "cloud-hypervisor"
	apiVersion        = "v1"
)

// Driver implements the vmm.Driver interface for Cloud-Hypervisor.
type Driver struct {
	binaryPath string
	dataDir    string
}

// NewDriver creates a new Cloud-Hypervisor driver.
func NewDriver(binaryPath, dataDir string) *Driver {
	if binaryPath == "" {
		binaryPath = defaultBinaryName
	}
	return &Driver{
		binaryPath: binaryPath,
		dataDir:    dataDir,
	}
}

// Name returns the driver name.
func (d *Driver) Name() string {
	return "cloud-hypervisor"
}

// IsAvailable checks if cloud-hypervisor binary exists.
func (d *Driver) IsAvailable() bool {
	_, err := exec.LookPath(d.binaryPath)
	return err == nil
}

// Version returns the cloud-hypervisor version.
func (d *Driver) Version() (string, error) {
	cmd := exec.Command(d.binaryPath, "--version")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get version: %w", err)
	}
	return string(bytes.TrimSpace(output)), nil
}

// Create creates a new microVM using Cloud-Hypervisor.
func (d *Driver) Create(ctx context.Context, config vmm.VMConfig) (vmm.VM, error) {
	if err := d.ensureDataDir(config.ID); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	apiSocket := d.getAPISocketPath(config.ID)
	vm := &cloudHypervisorVM{
		id:        config.ID,
		driver:    d,
		config:    config,
		apiSocket: apiSocket,
		state:     vmm.VMStatePending,
	}

	return vm, nil
}

// ensureDataDir creates the data directory for a VM.
func (d *Driver) ensureDataDir(vmID string) error {
	vmDir := filepath.Join(d.dataDir, "vms", vmID)
	return os.MkdirAll(vmDir, 0755)
}

// getAPISocketPath returns the path to the API socket for a VM.
func (d *Driver) getAPISocketPath(vmID string) string {
	return filepath.Join(d.dataDir, "vms", vmID, "api.sock")
}

// cloudHypervisorVM implements the vmm.VM interface.
type cloudHypervisorVM struct {
	id         string
	driver     *Driver
	config     vmm.VMConfig
	apiSocket  string
	cmd        *exec.Cmd
	pid        int
	state      vmm.VMState
	mu         sync.RWMutex
	waitDone   chan error
	httpClient *http.Client
}

// ID returns the VM ID.
func (vm *cloudHypervisorVM) ID() string {
	return vm.id
}

// Start starts the VM.
func (vm *cloudHypervisorVM) Start(ctx context.Context) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	if vm.state == vmm.VMStateRunning {
		return fmt.Errorf("VM is already running")
	}

	// Create VM log directory
	vmDir := filepath.Join(vm.driver.dataDir, "vms", vm.id)
	if err := os.MkdirAll(vmDir, 0755); err != nil {
		return fmt.Errorf("failed to create VM directory: %w", err)
	}

	// Build args with serial parameter
	args := vm.buildArgs()

	// Create command
	vm.cmd = exec.Command(vm.driver.binaryPath, args...)
	vm.cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	// Set netns if specified
	if vm.config.NetNS != "" {
		vm.cmd.SysProcAttr.Cloneflags = syscall.CLONE_NEWNET
		// Note: Additional netns setup needed here
	}

	if err := vm.cmd.Start(); err != nil {
		vm.state = vmm.VMStateFailed
		return fmt.Errorf("failed to start VM: %w", err)
	}

	vm.pid = vm.cmd.Process.Pid
	vm.state = vmm.VMStateRunning

	log.Info("[VM.Start] VM %s started with PID %d, apiSocket=%s", vm.id, vm.pid, vm.apiSocket)

	// Start goroutine to wait for process
	vm.waitDone = make(chan error, 1)
	go func() {
		var waitErr error
		if err := vm.cmd.Wait(); err != nil {
			log.Warn("VM %s exited with error: %v", vm.id, err)
			waitErr = err
		}
		vm.mu.Lock()
		vm.state = vmm.VMStateStopped
		vm.mu.Unlock()
		vm.waitDone <- waitErr
		close(vm.waitDone)
	}()

	// Wait for API socket with shorter timeout and logging
	if err := vm.waitForAPI(ctx, 30*time.Second); err != nil {
		log.Error("[VM.Start] VM %s API not ready: %v, forcing stop", vm.id, err)
		vm.ForceStop(ctx)
		return fmt.Errorf("VM API not ready: %w", err)
	}

	// Create HTTP client after API socket is ready
	vm.createHTTPClient()
	log.Info("[VM.Start] VM %s API ready, VM started successfully", vm.id)

	return nil
}

// createHTTPClient creates HTTP client after socket is ready
func (vm *cloudHypervisorVM) createHTTPClient() {
	vm.httpClient = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("unix", vm.apiSocket)
			},
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 5,
			IdleConnTimeout:     30 * time.Second,
		},
		Timeout: 30 * time.Second,
	}
}

// Stop stops the VM gracefully.
func (vm *cloudHypervisorVM) Stop(ctx context.Context) error {
	vm.mu.RLock()
	state := vm.state
	vm.mu.RUnlock()

	log.Info("[VM.Stop] Stopping VM %s (current state: %s)", vm.id, state)

	if state != vmm.VMStateRunning {
		log.Warn("[VM.Stop] VM %s is not running (state: %s)", vm.id, state)
		return fmt.Errorf("VM is not running")
	}

	// Send shutdown via API
	log.Debug("[VM.Stop] Sending shutdown API call for VM %s", vm.id)
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := vm.sendAPICall(shutdownCtx, http.MethodPut, "/vm.shutdown", nil, 5*time.Second); err != nil {
		log.Warn("[VM.Stop] Failed to send shutdown API call for VM %s: %v, will try force stop", vm.id, err)
		return vm.ForceStop(ctx)
	}
	log.Debug("[VM.Stop] Shutdown API call sent successfully for VM %s", vm.id)

	// Wait for process to exit with timeout
	log.Debug("[VM.Stop] Waiting for VM %s process to exit (timeout: 30s)", vm.id)
	select {
	case <-ctx.Done():
		log.Warn("[VM.Stop] Context cancelled while waiting for VM %s to stop", vm.id)
		return ctx.Err()
	case <-time.After(30 * time.Second):
		log.Warn("[VM.Stop] VM %s shutdown timeout after 30s, forcing stop", vm.id)
		return vm.ForceStop(ctx)
	case err := <-vm.waitDone:
		log.Info("[VM.Stop] VM %s process has exited: %v", vm.id, err)
		vm.mu.Lock()
		vm.state = vmm.VMStateStopped
		vm.mu.Unlock()
		return nil
	}
}

// ForceStop forcefully stops the VM.
func (vm *cloudHypervisorVM) ForceStop(ctx context.Context) error {
	vm.mu.RLock()
	cmd := vm.cmd
	pid := vm.pid
	vm.mu.RUnlock()

	log.Info("[VM.ForceStop] Force stopping VM %s (PID: %d)", vm.id, pid)

	if cmd != nil && cmd.Process != nil {
		// First try SIGTERM
		log.Debug("[VM.ForceStop] Sending SIGTERM to VM %s process", vm.id)
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			log.Warn("[VM.ForceStop] Failed to send SIGTERM to VM %s: %v, trying SIGKILL", vm.id, err)
		}

		// Wait for process to exit with timeout
		vm.mu.RLock()
		waitDone := vm.waitDone
		vm.mu.RUnlock()

		if waitDone != nil {
			select {
			case <-waitDone:
				log.Debug("[VM.ForceStop] VM %s process exited after SIGTERM", vm.id)
				vm.mu.Lock()
				vm.state = vmm.VMStateStopped
				vm.mu.Unlock()
				// Close HTTP client
				if vm.httpClient != nil {
					vm.httpClient.CloseIdleConnections()
				}
				return nil
			case <-time.After(2 * time.Second):
				log.Warn("[VM.ForceStop] VM %s did not exit after SIGTERM, sending SIGKILL", vm.id)
			}
		} else {
			// waitDone is nil, process might have already exited
			log.Debug("[VM.ForceStop] VM %s waitDone is nil, process might have already exited", vm.id)
			// Still try to kill to be safe
			cmd.Process.Kill()
		}

		// Force kill with SIGKILL
		log.Debug("[VM.ForceStop] Sending SIGKILL to VM %s process", vm.id)
		if err := cmd.Process.Kill(); err != nil {
			// Check if process is already gone
			if err.Error() != "os: process already finished" {
				log.Warn("[VM.ForceStop] Failed to send SIGKILL to VM %s: %v", vm.id, err)
			}
		}

		// Wait for kill to complete
		if waitDone != nil {
			select {
			case <-waitDone:
				log.Debug("[VM.ForceStop] VM %s process killed successfully", vm.id)
			case <-time.After(5 * time.Second):
				log.Warn("[VM.ForceStop] VM %s process did not exit after SIGKILL", vm.id)
			}
		}

		// Try to kill using PID as fallback (in case process was reparented)
		if pid > 0 {
			process, err := os.FindProcess(pid)
			if err == nil {
				process.Kill()
			}
		}
	} else {
		log.Debug("[VM.ForceStop] VM %s has no process to stop", vm.id)
	}

	vm.mu.Lock()
	vm.state = vmm.VMStateStopped
	vm.mu.Unlock()

	// Close HTTP client
	if vm.httpClient != nil {
		vm.httpClient.CloseIdleConnections()
	}

	log.Info("[VM.ForceStop] VM %s force stopped", vm.id)
	return nil
}

// Pause pauses the VM.
func (vm *cloudHypervisorVM) Pause(ctx context.Context) error {
	vm.mu.RLock()
	state := vm.state
	vm.mu.RUnlock()

	if state != vmm.VMStateRunning {
		return fmt.Errorf("VM is not running")
	}

	if err := vm.sendAPICall(ctx, http.MethodPut, "/vm.pause", nil, 30*time.Second); err != nil {
		return fmt.Errorf("failed to pause VM: %w", err)
	}

	vm.mu.Lock()
	vm.state = vmm.VMStatePaused
	vm.mu.Unlock()

	return nil
}

// Resume resumes the VM.
func (vm *cloudHypervisorVM) Resume(ctx context.Context) error {
	vm.mu.RLock()
	state := vm.state
	vm.mu.RUnlock()

	if state != vmm.VMStatePaused {
		return fmt.Errorf("VM is not paused")
	}

	if err := vm.sendAPICall(ctx, http.MethodPut, "/vm.resume", nil, 30*time.Second); err != nil {
		return fmt.Errorf("failed to resume VM: %w", err)
	}

	vm.mu.Lock()
	vm.state = vmm.VMStateRunning
	vm.mu.Unlock()

	return nil
}

// Snapshot creates a snapshot of the VM.
func (vm *cloudHypervisorVM) Snapshot(ctx context.Context, path string) error {
	if vm.state != vmm.VMStatePaused {
		return fmt.Errorf("VM must be paused before snapshot")
	}

	body := map[string]interface{}{
		"destination_url": fmt.Sprintf("file://%s", path),
	}

	if err := vm.sendAPICall(ctx, http.MethodPut, "/snapshot/create", body, 60*time.Second); err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	return nil
}

// Restore restores the VM from a snapshot.
func (vm *cloudHypervisorVM) Restore(ctx context.Context, path string) error {
	body := map[string]interface{}{
		"source_url": fmt.Sprintf("file://%s", path),
	}

	if err := vm.sendAPICall(ctx, http.MethodPut, "/snapshot/restore", body, 60*time.Second); err != nil {
		return fmt.Errorf("failed to restore snapshot: %w", err)
	}

	vm.mu.Lock()
	vm.state = vmm.VMStateRunning
	vm.mu.Unlock()

	return nil
}

// Info returns information about the VM.
func (vm *cloudHypervisorVM) Info(ctx context.Context) (vmm.VMInfo, error) {
	vm.mu.RLock()
	state := vm.state
	pid := vm.pid
	vm.mu.RUnlock()

	info := vmm.VMInfo{
		ID:       vm.id,
		State:    state,
		PID:      pid,
		VCPUs:    vm.config.VCPUs,
		MemoryMB: vm.config.Memory.SizeMB,
	}

	if state == vmm.VMStateRunning {
		// Try to get actual info from API
		vmInfo, err := vm.getVMInfo(ctx)
		if err == nil {
			info = *vmInfo
		}
	}

	return info, nil
}

// Wait blocks until the VM stops.
func (vm *cloudHypervisorVM) Wait(ctx context.Context) error {
	vm.mu.RLock()
	waitDone := vm.waitDone
	vm.mu.RUnlock()

	if waitDone == nil {
		return fmt.Errorf("VM not started")
	}

	select {
	case <-waitDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// buildArgs builds command line arguments for cloud-hypervisor.
func (vm *cloudHypervisorVM) buildArgs() []string {
	// Build memory configuration
	memArgs := fmt.Sprintf("size=%dM", vm.config.Memory.SizeMB)

	if vm.config.Memory.Backend == vmm.MemoryBackendFile && vm.config.Memory.BackendPath != "" {
		memArgs += fmt.Sprintf(",file=%s", vm.config.Memory.BackendPath)
	}
	if vm.config.Memory.Shared {
		memArgs += ",shared=on"
	}

	// Build log file paths
	serialLogFile := filepath.Join(vm.driver.dataDir, "vms", vm.id, "console.log")
	chLogFile := filepath.Join(vm.driver.dataDir, "vms", vm.id, "cloud-hypervisor.log")

	// Build cmdline: if rootfs is set, add root parameter; otherwise use rdinit=/bin/sh
	cmdline := vm.config.Boot.Cmdline
	if vm.config.RootFS != "" {
		if cmdline != "" {
			cmdline += " root=/dev/vda1"
		} else {
			cmdline = "root=/dev/vda1"
		}
	} else {
		if cmdline != "" {
			cmdline += " rdinit=/bin/sh"
		} else {
			cmdline = "rdinit=/bin/sh"
		}
	}

	args := []string{
		"--api-socket", vm.apiSocket,
		"--cpus", fmt.Sprintf("boot=%d", vm.config.VCPUs),
		"--memory", memArgs,
		"--kernel", vm.config.Boot.KernelPath,
		"--cmdline", fmt.Sprintf("\"%s\"", cmdline),
		"--console", "off",
		"--serial", fmt.Sprintf("file=%s", serialLogFile),
		"--log-file", chLogFile,
		"-vv",
	}

	// Add initrd if specified
	if vm.config.Boot.InitrdPath != "" {
		args = append(args, "--initramfs", vm.config.Boot.InitrdPath)
	}

	// Add rootfs as virtio disk
	if vm.config.RootFS != "" {
		args = append(args, "--disk", fmt.Sprintf("path=%s", vm.config.RootFS))
	}

	// Add network if configured
	if vm.config.TapDevice != "" {
		netArg := fmt.Sprintf("tap=%s", vm.config.TapDevice)
		if vm.config.VMIP != "" {
			netArg += fmt.Sprintf(",ip=%s", vm.config.VMIP)
		}
		args = append(args, "--net", netArg)
	}

	return args
}

// waitForAPI waits for the API socket to become available.
func (vm *cloudHypervisorVM) waitForAPI(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	attempt := 0

	for time.Now().Before(deadline) {
		attempt++

		// Check if socket exists
		if _, err := os.Stat(vm.apiSocket); err == nil {
			// Try ping with short timeout
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if err := vm.pingAPI(pingCtx); err == nil {
				cancel()
				log.Debug("[VM.waitForAPI] VM %s API responded successfully on attempt %d", vm.id, attempt)
				return nil
			}
			cancel()
			log.Debug("[VM.waitForAPI] VM %s API ping failed: %v, retrying...", vm.id, err)
		} else {
			log.Debug("[VM.waitForAPI] VM %s API socket not found yet (attempt %d)", vm.id, attempt)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled while waiting for API: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
			continue
		}
	}

	return fmt.Errorf("timeout waiting for API after %d attempts (timeout: %v)", attempt, timeout)
}

// pingAPI pings the API to check if it's ready.
func (vm *cloudHypervisorVM) pingAPI(ctx context.Context) error {
	// Create a temporary HTTP client for ping
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("unix", vm.apiSocket)
			},
		},
		Timeout: 2 * time.Second,
	}
	defer client.CloseIdleConnections()

	url := fmt.Sprintf("http://localhost/api/%s/ping", apiVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create ping request: %w", err)
	}

	log.Debug("[VM.pingAPI] VM %s pinging: %s", vm.id, url)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ping request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ping failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	log.Debug("[VM.pingAPI] VM %s ping successful", vm.id)
	return nil
}

// getVMInfo retrieves VM information from the API.
func (vm *cloudHypervisorVM) getVMInfo(ctx context.Context) (*vmm.VMInfo, error) {
	// Simplified - in production, parse actual API response
	return &vmm.VMInfo{
		ID:    vm.id,
		State: vmm.VMStateRunning,
		PID:   vm.pid,
	}, nil
}

// sendAPICall sends an HTTP request to the VM API via Unix socket.
func (vm *cloudHypervisorVM) sendAPICall(ctx context.Context, method, path string, body interface{}, timeout time.Duration) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	url := fmt.Sprintf("http://localhost/api/%s%s", apiVersion, path)

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	log.Debug("[VM.sendAPICall] VM %s sending request: %s %s", vm.id, method, url)

	resp, err := vm.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send API request %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}
