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

	"github.com/microvm/sandbox/pkg/vmm"
)

const (
	defaultAPIPort    = 8000
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

// List returns all VMs managed by this driver.
func (d *Driver) List(ctx context.Context) ([]vmm.VMInfo, error) {
	// Look for active VMs in data directory
	entries, err := os.ReadDir(d.dataDir)
	if err != nil {
		return nil, err
	}

	var vms []vmm.VMInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		vm := &cloudHypervisorVM{
			id:        entry.Name(),
			driver:    d,
			apiSocket: d.getAPISocketPath(entry.Name()),
		}

		info, err := vm.Info(ctx)
		if err != nil {
			continue
		}
		vms = append(vms, info)
	}

	return vms, nil
}

func (d *Driver) ensureDataDir(id string) error {
	dir := filepath.Join(d.dataDir, id)
	return os.MkdirAll(dir, 0755)
}

func (d *Driver) getAPISocketPath(id string) string {
	return filepath.Join(d.dataDir, id, "api.sock")
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

// SetState sets the VM state (used when loading from storage).
func (vm *cloudHypervisorVM) SetState(state vmm.VMState) {
	vm.state = state
}

// SetPID sets the VM PID (used when loading from storage).
func (vm *cloudHypervisorVM) SetPID(pid int) {
	vm.pid = pid
}

// Start starts the VM.
func (vm *cloudHypervisorVM) Start(ctx context.Context) error {
	if vm.state == vmm.VMStateRunning {
		return fmt.Errorf("VM is already running")
	}

	args := vm.buildArgs()

	// Build log file path
	logFile := filepath.Join(vm.driver.dataDir, vm.id, "vm.log")

	// Ensure log directory exists
	if err := os.MkdirAll(filepath.Dir(logFile), 0755); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	// Open log file
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer f.Close()

	// Log detailed configuration and full command
	vm.logDetailedConfig(f)
	cmdStr := vm.driver.binaryPath + " " + strings.Join(args, " ")
	fmt.Fprintf(f, "[%s] Starting VM with command: %s\n", time.Now().Format("2006-01-02 15:04:05"), cmdStr)
	f.Sync()

	// Create stdout/stderr log files separately to avoid race conditions
	stdoutFile := filepath.Join(vm.driver.dataDir, vm.id, "vm.stdout.log")
	stderrFile := filepath.Join(vm.driver.dataDir, vm.id, "vm.stderr.log")

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

	// Create command (don't use CommandContext - we want the VM to keep running)
	vm.cmd = exec.Command(vm.driver.binaryPath, args...)
	vm.cmd.Stdout = stdoutF
	vm.cmd.Stderr = stderrF

	// Set up process attributes for proper daemonization
	vm.cmd.SysProcAttr = &syscall.SysProcAttr{
		// Create new session to detach from terminal
		Setsid: true,
	}

	fmt.Fprintf(f, "[%s] Executing: %s\n", time.Now().Format("2006-01-02 15:04:05"), vm.driver.binaryPath)
	fmt.Fprintf(f, "[%s] Args: %v\n", time.Now().Format("2006-01-02 15:04:05"), args)
	f.Sync()

	if err := vm.cmd.Start(); err != nil {
		fmt.Fprintf(f, "[%s] Failed to start VM: %v\n", time.Now().Format("2006-01-02 15:04:05"), err)
		vm.state = vmm.VMStateFailed
		return fmt.Errorf("failed to start VM: %w", err)
	}

	vm.pid = vm.cmd.Process.Pid
	vm.state = vmm.VMStateRunning

	fmt.Fprintf(f, "[%s] VM started with PID: %d\n", time.Now().Format("2006-01-02 15:04:05"), vm.pid)
	f.Sync()

	// Channel to track process exit
	processDone := make(chan error, 1)

	// Start a goroutine to wait for the process to avoid zombie
	// Reopen log file in goroutine to avoid writing to closed file descriptor
	go func(vmID string, pid int) {
		// Wait for process to finish
		waitErr := vm.cmd.Wait()
		processDone <- waitErr

		// Reopen log file for writing exit status
		logFile := filepath.Join(vm.driver.dataDir, vmID, "vm.log")
		logF, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err == nil {
			defer logF.Close()
			if waitErr != nil {
				fmt.Fprintf(logF, "[%s] VM process (PID: %d) exited with error: %v\n", time.Now().Format("2006-01-02 15:04:05"), pid, waitErr)
			} else {
				fmt.Fprintf(logF, "[%s] VM process (PID: %d) exited successfully\n", time.Now().Format("2006-01-02 15:04:05"), pid)
			}
		}
		vm.state = vmm.VMStateStopped
	}(vm.id, vm.pid)

	// Give the process a moment to start before checking API
	time.Sleep(500 * time.Millisecond)

	// Check if process exited early
	select {
	case waitErr := <-processDone:
		// Process exited before API was ready
		vm.state = vmm.VMStateFailed
		// Read stderr for error details
		stderrContent, _ := os.ReadFile(stderrFile)
		if len(stderrContent) > 0 {
			return fmt.Errorf("VM process exited early: %v, stderr: %s", waitErr, string(stderrContent))
		}
		return fmt.Errorf("VM process exited early: %v", waitErr)
	default:
		// Process still running, continue to API check
	}

	// Wait for API socket to be ready
	if err := vm.waitForAPI(ctx, 30*time.Second); err != nil {
		vm.ForceStop(ctx)
		return fmt.Errorf("VM API not ready: %w", err)
	}

	return nil
}

// Stop stops the VM gracefully.
func (vm *cloudHypervisorVM) Stop(ctx context.Context) error {
	// Check if VM is actually running by checking the process
	if vm.state != vmm.VMStateRunning {
		return fmt.Errorf("VM is not running (current state: %s)", vm.state)
	}

	// Try to find the process by PID
	var process *os.Process
	var err error

	if vm.cmd != nil && vm.cmd.Process != nil {
		// Use the process from cmd if available (same session)
		process = vm.cmd.Process
	} else if vm.pid > 0 {
		// Try to find process by PID (loaded from storage)
		process, err = os.FindProcess(vm.pid)
		if err != nil {
			vm.state = vmm.VMStateStopped
			return fmt.Errorf("VM process with PID %d not found: %w", vm.pid, err)
		}
	} else {
		vm.state = vmm.VMStateStopped
		return fmt.Errorf("VM process information not available")
	}

	// Check if process is still alive
	if err := process.Signal(syscall.Signal(0)); err != nil {
		// Process is not running, update state
		vm.state = vmm.VMStateStopped
		return fmt.Errorf("VM process is not running (may have crashed or been killed)")
	}

	// Send shutdown signal via API
	if err := vm.sendAPICall(ctx, http.MethodPut, "/vm.shutdown", nil); err != nil {
		return fmt.Errorf("failed to shutdown VM: %w", err)
	}

	// Wait for process to exit
	done := make(chan error, 1)
	go func() {
		// Try to wait using cmd if available, otherwise just wait and check
		if vm.cmd != nil {
			done <- vm.cmd.Wait()
		} else {
			// Poll for process exit
			for {
				time.Sleep(100 * time.Millisecond)
				if err := process.Signal(syscall.Signal(0)); err != nil {
					done <- nil
					return
				}
			}
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		vm.state = vmm.VMStateStopped
		return err
	case <-time.After(30 * time.Second):
		// Force stop if graceful shutdown takes too long
		return vm.ForceStop(ctx)
	}
}

// ForceStop forcefully stops the VM.
func (vm *cloudHypervisorVM) ForceStop(ctx context.Context) error {
	// Try to find the process by PID
	var process *os.Process
	var err error

	if vm.cmd != nil && vm.cmd.Process != nil {
		// Use the process from cmd if available (same session)
		process = vm.cmd.Process
	} else if vm.pid > 0 {
		// Try to find process by PID (loaded from storage)
		process, err = os.FindProcess(vm.pid)
		if err != nil {
			// Process not found, assume already stopped
			vm.state = vmm.VMStateStopped
			return nil
		}
	} else {
		// No process information available
		vm.state = vmm.VMStateStopped
		return nil
	}

	// Try graceful termination first
	if err := process.Signal(syscall.SIGTERM); err != nil {
		// Process might already be dead
		vm.state = vmm.VMStateStopped
		return nil
	}

	// Give it a moment to terminate gracefully
	time.Sleep(2 * time.Second)

	// Force kill if still running
	_ = process.Kill()

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
func (vm *cloudHypervisorVM) Snapshot(ctx context.Context, config vmm.SnapshotConfig) error {
	if vm.state != vmm.VMStateRunning && vm.state != vmm.VMStatePaused {
		return fmt.Errorf("VM must be running or paused to create snapshot")
	}

	prevState := vm.state
	vm.state = vmm.VMStateSnapshot

	snapshotReq := map[string]interface{}{
		"destination_url": config.Destination,
	}

	if err := vm.sendAPICall(ctx, http.MethodPut, "/vm.snapshot", snapshotReq); err != nil {
		vm.state = prevState
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	vm.state = prevState
	return nil
}

// Restore restores the VM from a snapshot.
func (vm *cloudHypervisorVM) Restore(ctx context.Context, config vmm.RestoreConfig) error {
	if vm.state != vmm.VMStateStopped {
		return fmt.Errorf("VM must be stopped before restore")
	}

	vm.state = vmm.VMStateRestoring

	restoreReq := map[string]interface{}{
		"source_url": config.Source,
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
	// Check if the VM process is actually running
	if vm.state == vmm.VMStateRunning {
		var process *os.Process
		var err error

		if vm.cmd != nil && vm.cmd.Process != nil {
			process = vm.cmd.Process
		} else if vm.pid > 0 {
			process, err = os.FindProcess(vm.pid)
			if err != nil {
				// Process not found, update state
				vm.state = vmm.VMStateStopped
			}
		}

		if process != nil {
			// Check if process is still alive
			if err := process.Signal(syscall.Signal(0)); err != nil {
				// Process is not running, update state
				vm.state = vmm.VMStateStopped
			}
		}
	}

	info := vmm.VMInfo{
		ID:         vm.id,
		State:      vm.state,
		PID:        vm.pid,
		VCPUs:      vm.config.VCPUs,
		MemoryMB:   vm.config.Memory.SizeMB,
		MemoryType: vm.config.Memory.Backend,
	}

	if vm.state == vmm.VMStateRunning {
		// Try to get actual info from API
		vmInfo, err := vm.getVMInfo(ctx)
		if err == nil {
			info.VCPUs = vmInfo.VCPUs
			info.MemoryMB = vmInfo.MemoryMB
		}
	}

	return info, nil
}

// AttachConsole attaches to the VM console.
func (vm *cloudHypervisorVM) AttachConsole(ctx context.Context) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("console attachment not implemented")
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
	if vm.config.Memory.Hugepages {
		memArgs += ",hugepages=on"
	}
	if vm.config.Memory.Mergeable {
		memArgs += ",mergeable=on"
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

	// Add rootfs as the first virtio disk (vda)
	if vm.config.RootFS.Path != "" {
		ro := ""
		if vm.config.RootFS.ReadOnly {
			ro = ",readonly=on"
		}
		args = append(args, "--disk", fmt.Sprintf("path=%s%s", vm.config.RootFS.Path, ro))
	}

	// Add additional disks (will be vdb, vdc, etc.)
	for _, disk := range vm.config.Disks {
		ro := ""
		if disk.ReadOnly {
			ro = ",readonly=on"
		}
		args = append(args, "--disk", fmt.Sprintf("path=%s%s", disk.Path, ro))
	}

	// Add network if configured
	if vm.config.Network.TapDevice != "" {
		args = append(args, "--net", fmt.Sprintf("tap=%s,mac=%s,ip=%s",
			vm.config.Network.TapDevice,
			vm.config.Network.MACAddr,
			vm.config.Network.IPAddr))
	}

	// Disable console to avoid interfering with terminal
	args = append(args, "--console", "null")

	return args
}

// logDetailedConfig logs detailed VM configuration to the log file.
func (vm *cloudHypervisorVM) logDetailedConfig(f *os.File) {
	timestamp := time.Now().Format("2006-01-02 15:04:05")

	fmt.Fprintf(f, "[%s] ========== VM Configuration ==========\n", timestamp)
	fmt.Fprintf(f, "[%s] VM ID: %s\n", timestamp, vm.id)
	fmt.Fprintf(f, "[%s] API Socket: %s\n", timestamp, vm.apiSocket)
	fmt.Fprintf(f, "[%s] VCPUs: %d\n", timestamp, vm.config.VCPUs)
	fmt.Fprintf(f, "[%s] Memory: %d MB\n", timestamp, vm.config.Memory.SizeMB)
	fmt.Fprintf(f, "[%s] Memory Backend: %s\n", timestamp, vm.config.Memory.Backend)
	if vm.config.Memory.BackendPath != "" {
		fmt.Fprintf(f, "[%s] Memory Backend Path: %s\n", timestamp, vm.config.Memory.BackendPath)
	}
	fmt.Fprintf(f, "[%s] Kernel Path: %s\n", timestamp, vm.config.Boot.KernelPath)
	if vm.config.Boot.InitrdPath != "" {
		fmt.Fprintf(f, "[%s] Initrd Path: %s\n", timestamp, vm.config.Boot.InitrdPath)
	}
	fmt.Fprintf(f, "[%s] Kernel Cmdline: %s\n", timestamp, vm.config.Boot.Cmdline)
	if vm.config.RootFS.Path != "" {
		fmt.Fprintf(f, "[%s] RootFS Path: %s\n", timestamp, vm.config.RootFS.Path)
		fmt.Fprintf(f, "[%s] RootFS ReadOnly: %v\n", timestamp, vm.config.RootFS.ReadOnly)
	}
	if len(vm.config.Disks) > 0 {
		fmt.Fprintf(f, "[%s] Additional Disks:\n", timestamp)
		for i, disk := range vm.config.Disks {
			fmt.Fprintf(f, "[%s]   Disk %d: path=%s, readonly=%v\n", timestamp, i, disk.Path, disk.ReadOnly)
		}
	}
	if vm.config.Network.TapDevice != "" {
		fmt.Fprintf(f, "[%s] Network:\n", timestamp)
		fmt.Fprintf(f, "[%s]   Tap Device: %s\n", timestamp, vm.config.Network.TapDevice)
		fmt.Fprintf(f, "[%s]   MAC Address: %s\n", timestamp, vm.config.Network.MACAddr)
		fmt.Fprintf(f, "[%s]   IP Address: %s\n", timestamp, vm.config.Network.IPAddr)
	}
	fmt.Fprintf(f, "[%s] ======================================\n", timestamp)
}

// waitForAPI waits for the API socket to become available.
func (vm *cloudHypervisorVM) waitForAPI(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error

	logFile := filepath.Join(vm.driver.dataDir, vm.id, "vm.log")
	f, _ := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if f != nil {
		defer f.Close()
	}

	for time.Now().Before(deadline) {
		if _, err := os.Stat(vm.apiSocket); err == nil {
			// Try to ping the API
			if err := vm.pingAPI(ctx); err == nil {
				if f != nil {
					fmt.Fprintf(f, "[%s] API is ready\n", time.Now().Format("2006-01-02 15:04:05"))
				}
				return nil
			} else {
				lastErr = err
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}

	if f != nil {
		fmt.Fprintf(f, "[%s] Timeout waiting for API at %s (PID: %d), last error: %v\n",
			time.Now().Format("2006-01-02 15:04:05"), vm.apiSocket, vm.pid, lastErr)
	}
	return fmt.Errorf("timeout waiting for API at %s (PID: %d)", vm.apiSocket, vm.pid)
}

// pingAPI checks if the API is responsive.
func (vm *cloudHypervisorVM) pingAPI(ctx context.Context) error {
	// Connect to Unix domain socket
	conn, err := net.Dial("unix", vm.apiSocket)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Send HTTP request over Unix socket
	req := "GET /api/v1/vm.info HTTP/1.0\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		return err
	}

	// Read response
	buf := make([]byte, 1024)
	_, err = conn.Read(buf)
	if err != nil && err != io.EOF {
		return err
	}

	return nil
}

// sendAPICall sends an HTTP request to the VM API.
func (vm *cloudHypervisorVM) sendAPICall(ctx context.Context, method, path string, body interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}

	// For now, this is a placeholder - actual Unix socket HTTP client implementation needed
	// In production, you'd use a proper Unix socket HTTP client
	_ = method
	_ = path
	_ = bodyReader

	return fmt.Errorf("API calls not fully implemented - requires Unix socket HTTP client")
}

// getVMInfo retrieves VM information from the API.
func (vm *cloudHypervisorVM) getVMInfo(ctx context.Context) (*vmm.VMInfo, error) {
	// Placeholder - would make actual API call
	return &vmm.VMInfo{
		ID:       vm.id,
		VCPUs:    vm.config.VCPUs,
		MemoryMB: vm.config.Memory.SizeMB,
	}, nil
}
