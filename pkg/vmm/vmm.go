// Package vmm defines the interface for Virtual Machine Monitor implementations.
package vmm

import (
	"context"
	"io"
)

// MemoryBackendType defines the type of memory backend.
type MemoryBackendType string

const (
	MemoryBackendAnonymous MemoryBackendType = "anonymous"
	MemoryBackendFile      MemoryBackendType = "file"
)

// MemoryConfig defines memory configuration with backend support.
type MemoryConfig struct {
	SizeMB      uint32            // Memory size in MB
	Backend     MemoryBackendType // Memory backend type: anonymous, file
	BackendPath string            // Path to backend file (for file backend)
	Shared      bool              // Whether memory is shared between host and guest
	Hugepages   bool              // Use hugepages for memory
	Mergeable   bool              // Enable kernel samepage merging
}

// BootConfig defines the boot configuration for the VM.
// The VM boots using kernel + initrd, with rootfs mounted as virtio disk.
type BootConfig struct {
	KernelPath  string // Path to kernel image (vmlinux or bzImage)
	InitrdPath  string // Path to initrd/initramfs image
	Cmdline     string // Kernel command line, should include root=/dev/vda1 or similar
	SnapshotKey string // Snapshot key for kernel image (internal use)
	MountPath   string // Mount path for kernel snapshot (internal use)
}

// VMConfig contains configuration for creating a new microVM.
type VMConfig struct {
	ID       string
	VCPUs    uint32
	Memory   MemoryConfig // Memory configuration with backend
	Boot     BootConfig   // Boot configuration (kernel + initrd)
	RootFS   DiskConfig   // Root filesystem passed as virtio disk
	Network  NetworkConfig
	Disks    []DiskConfig // Additional disks
	LogLevel string
}

// NetworkConfig defines network configuration for the VM.
type NetworkConfig struct {
	TapDevice string
	IPAddr    string
	MACAddr   string
	Gateway   string
}

// DiskConfig defines a disk attachment configuration.
type DiskConfig struct {
	Path     string
	ReadOnly bool
	Format   string // raw, qcow2, etc.
}

// SnapshotConfig defines snapshot parameters.
type SnapshotConfig struct {
	Destination string // Path or destination for snapshot
	Compress    bool
}

// RestoreConfig defines restore parameters.
type RestoreConfig struct {
	Source string // Path to snapshot
}

// VMState represents the current state of a VM.
type VMState string

const (
	VMStatePending   VMState = "pending"
	VMStateRunning   VMState = "running"
	VMStatePaused    VMState = "paused"
	VMStateStopped   VMState = "stopped"
	VMStateFailed    VMState = "failed"
	VMStateSnapshot  VMState = "snapshotting"
	VMStateRestoring VMState = "restoring"
)

// VMInfo contains runtime information about a VM.
type VMInfo struct {
	ID         string
	State      VMState
	PID        int
	VCPUs      uint32
	MemoryMB   uint32
	MemoryType MemoryBackendType
	Uptime     string
	IPAddr     string
}

// Driver is the interface that all VMM implementations must satisfy.
type Driver interface {
	// Name returns the VMM name (e.g., "cloud-hypervisor", "firecracker").
	Name() string

	// Create creates a new microVM with the given configuration.
	Create(ctx context.Context, config VMConfig) (VM, error)

	// List returns information about all managed VMs.
	List(ctx context.Context) ([]VMInfo, error)

	// IsAvailable checks if the VMM binary is available on the system.
	IsAvailable() bool

	// Version returns the VMM version string.
	Version() (string, error)
}

// VM represents a running microVM instance.
type VM interface {
	// ID returns the unique identifier for this VM.
	ID() string

	// Start starts the VM.
	Start(ctx context.Context) error

	// Stop stops the VM gracefully.
	Stop(ctx context.Context) error

	// ForceStop forcefully stops the VM.
	ForceStop(ctx context.Context) error

	// Pause pauses the VM.
	Pause(ctx context.Context) error

	// Resume resumes a paused VM.
	Resume(ctx context.Context) error

	// Snapshot creates a snapshot of the VM state.
	Snapshot(ctx context.Context, config SnapshotConfig) error

	// Restore restores the VM from a snapshot.
	Restore(ctx context.Context, config RestoreConfig) error

	// Info returns current VM information.
	Info(ctx context.Context) (VMInfo, error)

	// AttachConsole attaches to the VM console.
	AttachConsole(ctx context.Context) (io.ReadWriteCloser, error)

	// Wait blocks until the VM stops.
	Wait(ctx context.Context) error
}

// Factory creates appropriate VMM drivers based on configuration.
type Factory struct {
	drivers map[string]Driver
}

// NewFactory creates a new VMM driver factory.
func NewFactory() *Factory {
	return &Factory{
		drivers: make(map[string]Driver),
	}
}

// Register registers a VMM driver.
func (f *Factory) Register(name string, driver Driver) {
	f.drivers[name] = driver
}

// Get returns the driver with the given name.
func (f *Factory) Get(name string) (Driver, bool) {
	driver, ok := f.drivers[name]
	return driver, ok
}

// ListAvailable returns all available drivers on the current system.
func (f *Factory) ListAvailable() []Driver {
	var available []Driver
	for _, driver := range f.drivers {
		if driver.IsAvailable() {
			available = append(available, driver)
		}
	}
	return available
}
