// Package sandbox provides high-level sandbox management.
package sandbox

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/microvm/sandbox/pkg/image"
	"github.com/microvm/sandbox/pkg/vmm"
)

// Manager handles the lifecycle of multiple sandboxes.
type Manager struct {
	factory       *vmm.Factory
	sandboxes     map[string]vmm.VM
	rootFSManager *image.RootFSManager
	store         *Store
	dataDir       string
	mu            sync.RWMutex
}

// Config contains manager configuration.
type Config struct {
	DataDir             string
	DefaultVMM          string
	ContainerdAddress   string
	ContainerdNamespace string
	Snapshotter         string
}

// NewManager creates a new sandbox manager.
func NewManager(config Config, factory *vmm.Factory) (*Manager, error) {
	// Initialize rootfs manager if containerd address is provided
	var rootFSManager *image.RootFSManager
	if config.ContainerdAddress != "" {
		client, err := image.NewClient(
			config.ContainerdAddress,
			config.ContainerdNamespace,
			config.Snapshotter,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create image client: %w", err)
		}
		rootFSManager = image.NewRootFSManager(client, config.DataDir)
	}

	// Initialize metadata store
	store := NewStore(config.DataDir)

	return &Manager{
		factory:       factory,
		sandboxes:     make(map[string]vmm.VM),
		rootFSManager: rootFSManager,
		store:         store,
		dataDir:       config.DataDir,
	}, nil
}

// Close closes the manager and releases resources.
func (m *Manager) Close() error {
	if m.rootFSManager != nil {
		// TODO: Cleanup all active rootfs snapshots
	}
	return nil
}

// CreateOptions contains options for creating a sandbox.
type CreateOptions struct {
	VMM       string // VMM driver to use (default: cloud-hypervisor)
	VCPUs     uint32
	Memory    vmm.MemoryConfig // Memory configuration with backend
	Network   vmm.NetworkConfig
	Disks     []vmm.DiskConfig // Additional disks
	AutoStart bool
	Image     string         // OCI image reference containing kernel and initrd
	RootFS    vmm.DiskConfig // Root filesystem disk (optional if using image)
	Boot      vmm.BootConfig // Boot configuration (cmdline only when using image)
}

// Create creates a new sandbox with the given options.
// If KernelImage is specified, it will:
// 1. Pull the OCI image
// 2. Create a snapshot (rootfs) from the image
// 3. Extract kernel and initrd paths from the rootfs
// 4. Use these paths to start the VM
func (m *Manager) Create(ctx context.Context, id string, opts CreateOptions) (vmm.VM, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sandboxes[id]; exists {
		return nil, fmt.Errorf("sandbox %s already exists", id)
	}

	// Use default VMM if not specified
	vmmName := opts.VMM
	if vmmName == "" {
		vmmName = "cloud-hypervisor"
	}

	driver, ok := m.factory.Get(vmmName)
	if !ok {
		return nil, fmt.Errorf("VMM driver %s not found", vmmName)
	}

	var bootConfig vmm.BootConfig
	var rootFSConfig vmm.DiskConfig

	// If image is specified, prepare rootfs from OCI image
	if opts.Image != "" {
		if m.rootFSManager == nil {
			return nil, fmt.Errorf("rootfs manager not initialized, cannot use image")
		}

		// Prepare rootfs from OCI image
		rootfs, err := m.rootFSManager.PrepareRootFS(ctx, opts.Image, id)
		if err != nil {
			return nil, fmt.Errorf("failed to prepare rootfs from image %s: %w", opts.Image, err)
		}

		// Set boot configuration from rootfs
		bootConfig = vmm.BootConfig{
			KernelPath:  rootfs.KernelPath,
			InitrdPath:  rootfs.InitrdPath,
			SnapshotKey: rootfs.SnapshotKey,
			MountPath:   rootfs.MountPath,
		}

		// If no rootfs disk specified, we can use the snapshot mount as the rootfs
		// This requires the VMM to support mounting a directory as a disk
		// For now, we require the user to provide a rootfs disk or use the existing one
		rootFSConfig = opts.RootFS
	} else {
		// Use traditional boot configuration
		// This requires manual specification of kernel and initrd paths
		return nil, fmt.Errorf("kernel image is required (use --kernel-image)")
	}

	vmConfig := vmm.VMConfig{
		ID:      id,
		VCPUs:   opts.VCPUs,
		Memory:  opts.Memory,
		Boot:    bootConfig,
		RootFS:  rootFSConfig,
		Network: opts.Network,
		Disks:   opts.Disks,
	}

	vm, err := driver.Create(ctx, vmConfig)
	if err != nil {
		// Cleanup rootfs snapshot if created
		if bootConfig.SnapshotKey != "" && m.rootFSManager != nil {
			rootfs := &image.RootFS{
				SnapshotKey: bootConfig.SnapshotKey,
				MountPath:   bootConfig.MountPath,
			}
			_ = rootfs.Cleanup(ctx, m.rootFSManager.Snapshotter())
		}
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	m.sandboxes[id] = vm

	// Save metadata to store
	meta := &SandboxMeta{
		ID:          id,
		VMM:         vmmName,
		VCPUs:       opts.VCPUs,
		MemoryMB:    opts.Memory.SizeMB,
		Image:       opts.Image,
		RootFS:      opts.RootFS.Path,
		Cmdline:     bootConfig.Cmdline,
		State:       vmm.VMStatePending,
		SnapshotKey: bootConfig.SnapshotKey,
		MountPath:   bootConfig.MountPath,
		CreatedAt:   time.Now().Format(time.RFC3339),
	}
	if err := m.store.Save(meta); err != nil {
		// Log error but don't fail
		fmt.Printf("Warning: failed to save sandbox metadata: %v\n", err)
	}

	// Auto-start if requested
	if opts.AutoStart {
		if err := vm.Start(ctx); err != nil {
			// Cleanup on start failure
			_ = vm.ForceStop(ctx)
			delete(m.sandboxes, id)
			// Cleanup rootfs snapshot
			if bootConfig.SnapshotKey != "" && m.rootFSManager != nil {
				rootfs := &image.RootFS{
					SnapshotKey: bootConfig.SnapshotKey,
					MountPath:   bootConfig.MountPath,
				}
				_ = rootfs.Cleanup(ctx, m.rootFSManager.Snapshotter())
			}
			return nil, fmt.Errorf("failed to start VM: %w", err)
		}
	}

	return vm, nil
}

// Get retrieves a sandbox by ID.
func (m *Manager) Get(id string) (vmm.VM, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	vm, ok := m.sandboxes[id]
	if !ok {
		return nil, fmt.Errorf("sandbox %s not found", id)
	}

	return vm, nil
}

// List returns all managed sandboxes.
func (m *Manager) List(ctx context.Context) ([]vmm.VMInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	metas, err := m.store.List()
	if err != nil {
		return nil, err
	}

	var infos []vmm.VMInfo
	for _, meta := range metas {
		info := vmm.VMInfo{
			ID:       meta.ID,
			State:    meta.State,
			VCPUs:    meta.VCPUs,
			MemoryMB: meta.MemoryMB,
		}

		// Try to get runtime info if VM is in memory
		if vm, ok := m.sandboxes[meta.ID]; ok {
			if runtimeInfo, err := vm.Info(ctx); err == nil {
				info = runtimeInfo
			}
		}

		infos = append(infos, info)
	}

	return infos, nil
}

// Stop stops a sandbox gracefully.
func (m *Manager) Stop(ctx context.Context, id string, force bool) error {
	vm, err := m.Get(id)
	if err != nil {
		return err
	}

	if force {
		return vm.ForceStop(ctx)
	}

	return vm.Stop(ctx)
}

// Pause pauses a running sandbox.
func (m *Manager) Pause(ctx context.Context, id string) error {
	vm, err := m.Get(id)
	if err != nil {
		return err
	}

	return vm.Pause(ctx)
}

// Resume resumes a paused sandbox.
func (m *Manager) Resume(ctx context.Context, id string) error {
	vm, err := m.Get(id)
	if err != nil {
		return err
	}

	return vm.Resume(ctx)
}

// SnapshotOptions contains snapshot configuration.
type SnapshotOptions struct {
	Destination string
	Compress    bool
}

// Snapshot creates a snapshot of a sandbox.
func (m *Manager) Snapshot(ctx context.Context, id string, opts SnapshotOptions) error {
	vm, err := m.Get(id)
	if err != nil {
		return err
	}

	config := vmm.SnapshotConfig{
		Destination: opts.Destination,
		Compress:    opts.Compress,
	}

	return vm.Snapshot(ctx, config)
}

// RestoreOptions contains restore configuration.
type RestoreOptions struct {
	Source string
}

// Restore restores a sandbox from a snapshot.
func (m *Manager) Restore(ctx context.Context, id string, opts RestoreOptions) error {
	vm, err := m.Get(id)
	if err != nil {
		return err
	}

	config := vmm.RestoreConfig{
		Source: opts.Source,
	}

	return vm.Restore(ctx, config)
}

// Delete removes a sandbox from management.
func (m *Manager) Delete(ctx context.Context, id string, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	vm, ok := m.sandboxes[id]
	if !ok {
		return fmt.Errorf("sandbox %s not found", id)
	}

	info, err := vm.Info(ctx)
	if err == nil && info.State == vmm.VMStateRunning {
		if force {
			if err := vm.ForceStop(ctx); err != nil {
				return fmt.Errorf("failed to stop VM: %w", err)
			}
		} else {
			return fmt.Errorf("sandbox is running, stop it first or use force")
		}
	}

	// TODO: Cleanup rootfs snapshot if VM was created with kernel image
	// Need to track the snapshot key in the VM or sandbox metadata

	delete(m.sandboxes, id)

	// Delete metadata from store
	_ = m.store.Delete(id)

	return nil
}

// Wait blocks until the specified sandbox stops.
func (m *Manager) Wait(ctx context.Context, id string) error {
	vm, err := m.Get(id)
	if err != nil {
		return err
	}

	return vm.Wait(ctx)
}

// HealthCheck performs health checks on all sandboxes.
func (m *Manager) HealthCheck(ctx context.Context) map[string]error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	results := make(map[string]error)
	for id, vm := range m.sandboxes {
		_, err := vm.Info(ctx)
		results[id] = err
	}

	return results
}

// Watchdog runs a background goroutine that monitors sandbox health.
func (m *Manager) Watchdog(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			results := m.HealthCheck(ctx)
			for id, err := range results {
				if err != nil {
					// Log the error or handle unhealthy sandbox
					_ = id
				}
			}
		}
	}
}
