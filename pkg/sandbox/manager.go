// Package sandbox provides high-level sandbox management.
package sandbox

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/microvm/sandbox/pkg/vmm"
)

// Manager handles the lifecycle of multiple sandboxes.
type Manager struct {
	factory   *vmm.Factory
	sandboxes map[string]vmm.VM
	dataDir   string
	mu        sync.RWMutex
}

// Config contains manager configuration.
type Config struct {
	DataDir    string
	DefaultVMM string
}

// NewManager creates a new sandbox manager.
func NewManager(config Config, factory *vmm.Factory) *Manager {
	return &Manager{
		factory:   factory,
		sandboxes: make(map[string]vmm.VM),
		dataDir:   config.DataDir,
	}
}

// CreateOptions contains options for creating a sandbox.
type CreateOptions struct {
	VMM       string // VMM driver to use (default: cloud-hypervisor)
	VCPUs     uint32
	Memory    vmm.MemoryConfig // Memory configuration with backend
	Boot      vmm.BootConfig   // Boot configuration (kernel + initrd)
	RootFS    vmm.DiskConfig   // Root filesystem passed as virtio disk
	Network   vmm.NetworkConfig
	Disks     []vmm.DiskConfig // Additional disks
	AutoStart bool
}

// Create creates a new sandbox with the given options.
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

	vmConfig := vmm.VMConfig{
		ID:      id,
		VCPUs:   opts.VCPUs,
		Memory:  opts.Memory,
		Boot:    opts.Boot,
		RootFS:  opts.RootFS,
		Network: opts.Network,
		Disks:   opts.Disks,
	}

	vm, err := driver.Create(ctx, vmConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	m.sandboxes[id] = vm

	// Auto-start if requested
	if opts.AutoStart {
		if err := vm.Start(ctx); err != nil {
			// Cleanup on start failure
			_ = vm.ForceStop(ctx)
			delete(m.sandboxes, id)
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

	var infos []vmm.VMInfo
	for _, vm := range m.sandboxes {
		info, err := vm.Info(ctx)
		if err != nil {
			continue
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

	delete(m.sandboxes, id)
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
