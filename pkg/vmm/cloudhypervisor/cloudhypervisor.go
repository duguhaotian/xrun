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

// Start starts the VM.
func (vm *cloudHypervisorVM) Start(ctx context.Context) error {
	if vm.state == vmm.VMStateRunning {
		return fmt.Errorf("VM is already running")
	}

	args := vm.buildArgs()

	// Build log file path
	logFile := filepath.Join(vm.driver.dataDir, vm.id, "vm.log")

	// Open log file
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer f.Close()

	vm.cmd = exec.Command(vm.driver.binaryPath, args...)
	vm.cmd.Stdout = f
	vm.cmd.Stderr = f

	// Don't set Setpgid - it causes issues with some VMMs
	// The process will still run in background because we don't wait for it

	if err := vm.cmd.Start(); err != nil {
		vm.state = vmm.VMStateFailed
		return fmt.Errorf("failed to start VM: %w", err)
	}

	vm.pid = vm.cmd.Process.Pid
	vm.state = vmm.VMStateRunning

	// Wait for API socket to be ready
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

	// Send shutdown signal via API
	if err := vm.sendAPICall(ctx, http.MethodPut, "/vm.shutdown", nil); err != nil {
		return fmt.Errorf("failed to shutdown VM: %w", err)
	}

	// Wait for process to exit
	done := make(chan error, 1)
	go func() {
		done <- vm.cmd.Wait()
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
	if vm.cmd != nil && vm.cmd.Process != nil {
		// Try graceful termination first
		if err := vm.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			// Process might already be dead
			vm.state = vmm.VMStateStopped
			return nil
		}

		// Give it a moment to terminate gracefully
		time.Sleep(2 * time.Second)

		// Force kill if still running
		_ = vm.cmd.Process.Kill()
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
		"--cmdline", vm.config.Boot.Cmdline,
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

	if vm.config.LogLevel != "" {
		args = append(args, "--log-level", vm.config.LogLevel)
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

// waitForAPI waits for the API socket to become available.
func (vm *cloudHypervisorVM) waitForAPI(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if _, err := os.Stat(vm.apiSocket); err == nil {
			// Try to ping the API
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
