// Package vmm defines the interface for Virtual Machine Monitor implementations.
package vmm

import (
	"context"
)

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

// MemoryBackendType defines the type of memory backend.
type MemoryBackendType string

const (
	MemoryBackendAnonymous MemoryBackendType = "anonymous"
	MemoryBackendFile      MemoryBackendType = "file"
)

// MemoryConfig defines memory configuration with backend support.
type MemoryConfig struct {
	SizeMB      uint32            // Memory size in MB
	Backend     MemoryBackendType // Memory backend type
	BackendPath string            // Path to backend file (for file backend)
	Shared      bool              // Whether memory is shared between host and guest
}

// BootConfig defines the boot configuration for the VM.
type BootConfig struct {
	KernelPath string // Path to kernel image
	InitrdPath string // Path to initrd image
	Cmdline    string // Kernel command line
}

// VMConfig contains configuration for creating a new microVM.
type VMConfig struct {
	ID        string
	VCPUs     uint32
	Memory    MemoryConfig
	Boot      BootConfig
	RootFS    string // Path to rootfs disk
	NetNS     string // Network namespace path (optional)
	TapDevice string // Tap device name (optional)
	VMIP      string // VM IP address (optional)
	LogLevel  string
}

// VMInfo contains runtime information about a VM.
type VMInfo struct {
	ID       string
	State    VMState
	PID      int
	VCPUs    uint32
	MemoryMB uint32
	IPAddr   string
}

// Driver is the interface that all VMM implementations must satisfy.
type Driver interface {
	// Name returns the VMM name.
	Name() string

	// Create creates a new microVM with the given configuration.
	Create(ctx context.Context, config VMConfig) (VM, error)

	// IsAvailable checks if the VMM binary is available.
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
	Snapshot(ctx context.Context, destPath string) error

	// Restore restores the VM from a snapshot.
	Restore(ctx context.Context, sourcePath string) error

	// Info returns current VM information.
	Info(ctx context.Context) (VMInfo, error)

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
