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
	"strings"
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

func (d *Driver) ensureDataDir(id string) error {
	dir := filepath.Join(d.dataDir, "vms", id)
	return os.MkdirAll(dir, 0755)
}

func (d *Driver) getAPISocketPath(id string) string {
	return filepath.Join(d.dataDir, "vms", id, "api.sock")
}

// cloudHypervisorVM represents a Cloud-Hypervisor VM instance.
type cloudHypervisorVM struct {
	id        string
	driver    *Driver
	config    vmm.VMConfig
	apiSocket string
	cmd       *exec.Cmd
	state     vmm.VMState
	pid       int
}

// ID returns the VM ID.
func (vm *cloudHypervisorVM) ID() string {
	return vm.id
}

// Start starts the VM.
func (vm *cloudHypervisorVM) Start(ctx context.Context) error {
	if vm.state == vmm.VMStateRunning {
		return fmt.Errorf("VM is already running")
	}

	args := vm.buildArgs()

	// Build log file path
	logFile := filepath.Join(vm.driver.dataDir, "vms", vm.id, "vm.log")
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer f.Close()

	// Log command
	cmdStr := vm.driver.binaryPath + " " + strings.Join(args, " ")
	fmt.Fprintf(f, "[%s] Starting VM with command: %s\n", time.Now().Format("2006-01-02 15:04:05"), cmdStr)
	f.Sync()

	// Create stdout/stderr log files
	stdoutFile := filepath.Join(vm.driver.dataDir, "vms", vm.id, "vm.stdout.log")
	stderrFile := filepath.Join(vm.driver.dataDir, "vms", vm.id, "vm.stderr.log")

	stdoutF, err := os.OpenFile(stdoutFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open stdout log file: %w", err)
	}
	defer stdoutF.Close()

	stderrF, err := os.OpenFile(stderrFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open stderr log file: %w", err)
	}
	defer stderrF.Close()

	// Create command
	vm.cmd = exec.Command(vm.driver.binaryPath, args...)
	vm.cmd.Stdout = stdoutF
	vm.cmd.Stderr = stderrF
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

	// Start goroutine to wait for process
	go func() {
		if err := vm.cmd.Wait(); err != nil {
			log.Warn("VM %s exited with error: %v", vm.id, err)
		}
		vm.state = vmm.VMStateStopped
	}()

	// Wait for API socket
	if err := vm.waitForAPI(ctx, 30*time.Second); err != nil {
		vm.ForceStop(ctx)
		return fmt.Errorf("VM API not ready: %w", err)
	}

	return nil
}

// Stop stops the VM gracefully.
func (vm *cloudHypervisorVM) Stop(ctx context.Context) error {
	if vm.state != vmm.VMStateRunning {
		return fmt.Errorf("VM is not running")
	}

	// Send shutdown via API
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := vm.sendAPICall(shutdownCtx, http.MethodPut, "/vm.shutdown", nil); err != nil {
		return fmt.Errorf("failed to shutdown VM: %w", err)
	}

	// Wait for process to exit
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(30 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return vm.ForceStop(ctx)
		case <-ticker.C:
			if vm.state != vmm.VMStateRunning {
				return nil
			}
		}
	}
}

// ForceStop forcefully stops the VM.
func (vm *cloudHypervisorVM) ForceStop(ctx context.Context) error {
	if vm.cmd != nil && vm.cmd.Process != nil {
		vm.cmd.Process.Signal(syscall.SIGTERM)
		time.Sleep(2 * time.Second)
		vm.cmd.Process.Kill()
	}
	vm.state = vmm.VMStateStopped
	return nil
}

// Pause pauses the VM.
func (vm *cloudHypervisorVM) Pause(ctx context.Context) error {
	if vm.state != vmm.VMStateRunning {
		return fmt.Errorf("VM is not running")
	}

	if err := vm.sendAPICall(ctx, http.MethodPut, "/vm.pause", nil); err != nil {
		return fmt.Errorf("failed to pause VM: %w", err)
	}

	vm.state = vmm.VMStatePaused
	return nil
}

// Resume resumes a paused VM.
func (vm *cloudHypervisorVM) Resume(ctx context.Context) error {
	if vm.state != vmm.VMStatePaused {
		return fmt.Errorf("VM is not paused")
	}

	if err := vm.sendAPICall(ctx, http.MethodPut, "/vm.resume", nil); err != nil {
		return fmt.Errorf("failed to resume VM: %w", err)
	}

	vm.state = vmm.VMStateRunning
	return nil
}

// Snapshot creates a snapshot of the VM.
func (vm *cloudHypervisorVM) Snapshot(ctx context.Context, destPath string) error {
	if vm.state != vmm.VMStateRunning && vm.state != vmm.VMStatePaused {
		return fmt.Errorf("VM must be running or paused to create snapshot")
	}

	prevState := vm.state
	vm.state = vmm.VMStateSnapshot

	snapshotReq := map[string]interface{}{
		"destination_url": "file://" + destPath,
	}

	if err := vm.sendAPICall(ctx, http.MethodPut, "/vm.snapshot", snapshotReq); err != nil {
		vm.state = prevState
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	vm.state = prevState
	return nil
}

// Restore restores the VM from a snapshot.
func (vm *cloudHypervisorVM) Restore(ctx context.Context, sourcePath string) error {
	if vm.state != vmm.VMStateStopped {
		return fmt.Errorf("VM must be stopped before restore")
	}

	vm.state = vmm.VMStateRestoring

	restoreReq := map[string]interface{}{
		"source_url": "file://" + sourcePath,
	}

	if err := vm.sendAPICall(ctx, http.MethodPut, "/vm.restore", restoreReq); err != nil {
		vm.state = vmm.VMStateFailed
		return fmt.Errorf("failed to restore VM: %w", err)
	}

	vm.state = vmm.VMStateRunning
	return nil
}

// Info returns current VM information.
func (vm *cloudHypervisorVM) Info(ctx context.Context) (vmm.VMInfo, error) {
	info := vmm.VMInfo{
		ID:       vm.id,
		State:    vm.state,
		PID:      vm.pid,
		VCPUs:    vm.config.VCPUs,
		MemoryMB: vm.config.Memory.SizeMB,
	}

	if vm.state == vmm.VMStateRunning {
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
	if vm.cmd == nil {
		return fmt.Errorf("VM not started")
	}

	return vm.cmd.Wait()
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

	args := []string{
		"--api-socket", vm.apiSocket,
		"--cpus", fmt.Sprintf("boot=%d", vm.config.VCPUs),
		"--memory", memArgs,
		"--kernel", vm.config.Boot.KernelPath,
		"--cmdline", fmt.Sprintf("\"%s\"", vm.config.Boot.Cmdline),
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

	// Disable console
	args = append(args, "--console", "null")

	return args
}

// waitForAPI waits for the API socket to become available.
func (vm *cloudHypervisorVM) waitForAPI(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if _, err := os.Stat(vm.apiSocket); err == nil {
			if err := vm.pingAPI(ctx); err == nil {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}

	return fmt.Errorf("timeout waiting for API")
}

// pingAPI checks if the API is responsive.
func (vm *cloudHypervisorVM) pingAPI(ctx context.Context) error {
	conn, err := net.Dial("unix", vm.apiSocket)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := "GET /api/v1/vm.info HTTP/1.0\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		return err
	}

	buf := make([]byte, 1024)
	_, err = conn.Read(buf)
	return err
}

// sendAPICall sends an HTTP request to the VM API via Unix socket.
func (vm *cloudHypervisorVM) sendAPICall(ctx context.Context, method, path string, body interface{}) error {
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

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("unix", vm.apiSocket)
			},
		},
		Timeout: 30 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send API request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// getVMInfo retrieves VM information from the API.
func (vm *cloudHypervisorVM) getVMInfo(ctx context.Context) (*vmm.VMInfo, error) {
	// For now, return basic info
	return &vmm.VMInfo{
		ID:       vm.id,
		State:    vm.state,
		PID:      vm.pid,
		VCPUs:    vm.config.VCPUs,
		MemoryMB: vm.config.Memory.SizeMB,
	}, nil
}
