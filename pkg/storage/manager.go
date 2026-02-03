// Package storage provides storage management for microVM sandboxes.
package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/snapshots"
	"github.com/google/uuid"
	"github.com/microvm/sandbox/pkg/log"
)

const (
	// LabelMemorySnapshot marks snapshots as memory snapshots
	LabelMemorySnapshot = "xrun.microvm/memory-snapshot"
	// LabelVMID stores the VM ID in snapshot labels
	LabelVMID = "xrun.microvm/vm-id"
	// LabelSnapshotID stores the snapshot ID in labels
	LabelSnapshotID = "xrun.microvm/snapshot-id"
	// LabelCreatedAt stores creation time
	LabelCreatedAt = "xrun.microvm/created-at"
)

// Manager handles storage operations for sandboxes.
type Manager struct {
	snapshotter snapshots.Snapshotter
	namespace   string
	dataDir     string

	// Memory snapshot cache (key: snapshot-key)
	memorySnapshots map[string]*MemorySnapshotInfo
	mu              sync.RWMutex
}

// MemorySnapshotInfo holds information about a memory snapshot.
type MemorySnapshotInfo struct {
	SnapshotKey string
	VMID        string
	SnapshotID  string
	SizeMB      uint32
	CreatedAt   time.Time
	Path        string
}

// MemoryFile represents a memory backend file for a VM.
type MemoryFile struct {
	Path        string
	SnapshotKey string
	SizeMB      uint32
}

// NewManager creates a new storage manager.
func NewManager(snapshotter snapshots.Snapshotter, namespace, dataDir string) *Manager {
	return &Manager{
		snapshotter:     snapshotter,
		namespace:       namespace,
		dataDir:         dataDir,
		memorySnapshots: make(map[string]*MemorySnapshotInfo),
	}
}

// Init initializes the storage manager and loads existing memory snapshots.
func (m *Manager) Init(ctx context.Context) error {
	ctx = namespaces.WithNamespace(ctx, m.namespace)

	// Walk all snapshots and find memory snapshots
	walkFunc := func(ctx context.Context, info snapshots.Info) error {
		if info.Labels[LabelMemorySnapshot] == "true" {
			vmID := info.Labels[LabelVMID]
			snapshotID := info.Labels[LabelSnapshotID]

			m.memorySnapshots[info.Name] = &MemorySnapshotInfo{
				SnapshotKey: info.Name,
				VMID:        vmID,
				SnapshotID:  snapshotID,
				CreatedAt:   info.Created,
			}

			log.Info("Loaded memory snapshot: %s (VM: %s)", info.Name, vmID)
		}
		return nil
	}

	if err := m.snapshotter.Walk(ctx, walkFunc); err != nil {
		return fmt.Errorf("failed to walk snapshots: %w", err)
	}

	return nil
}

// CreateMemoryFile creates a memory backend file for a VM.
func (m *Manager) CreateMemoryFile(ctx context.Context, vmID string, sizeMB uint32) (*MemoryFile, error) {
	ctx = namespaces.WithNamespace(ctx, m.namespace)

	// Generate snapshot key
	snapshotKey := fmt.Sprintf("xrun-memory-%s-%s", vmID, uuid.New().String()[:8])

	// Create a snapshot with labels
	// Note: We add "containerd.io/gc.root" label to prevent GC of this active snapshot
	labels := map[string]string{
		LabelMemorySnapshot:              "true",
		LabelVMID:                        vmID,
		LabelCreatedAt:                   time.Now().Format(time.RFC3339),
		"containerd.io/gc.root":          "true",
		"containerd.io/gc.root.snapshot": "true",
	}

	opts := snapshots.WithLabels(labels)

	// Create empty snapshot (no parent)
	mounts, err := m.snapshotter.Prepare(ctx, snapshotKey, "", opts)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare memory snapshot: %w", err)
	}

	// Create mount directory
	mountPath := filepath.Join(m.dataDir, "snapshots", snapshotKey)
	if err := os.MkdirAll(mountPath, 0755); err != nil {
		m.snapshotter.Remove(ctx, snapshotKey)
		return nil, fmt.Errorf("failed to create mount directory: %w", err)
	}

	// Mount the snapshot
	if err := mount.All(mounts, mountPath); err != nil {
		os.RemoveAll(mountPath)
		m.snapshotter.Remove(ctx, snapshotKey)
		return nil, fmt.Errorf("failed to mount snapshot: %w", err)
	}

	// Create memory file
	memFilePath := filepath.Join(mountPath, "memory.file")
	if err := createSparseFile(memFilePath, int64(sizeMB)*1024*1024); err != nil {
		mount.UnmountAll(mountPath, 0)
		os.RemoveAll(mountPath)
		m.snapshotter.Remove(ctx, snapshotKey)
		return nil, fmt.Errorf("failed to create memory file: %w", err)
	}

	// Note: We don't commit here for 'run' operation.
	// The snapshot stays as an active layer (prepared but not committed).
	// This allows the VM to write to the memory file.
	// We add a special label to prevent GC of this active snapshot.

	// Add to cache
	m.mu.Lock()
	m.memorySnapshots[snapshotKey] = &MemorySnapshotInfo{
		SnapshotKey: snapshotKey,
		VMID:        vmID,
		SizeMB:      sizeMB,
		CreatedAt:   time.Now(),
		Path:        memFilePath,
	}
	m.mu.Unlock()

	return &MemoryFile{
		Path:        memFilePath,
		SnapshotKey: snapshotKey,
		SizeMB:      sizeMB,
	}, nil
}

// GetMemoryFile returns the memory file path for a VM.
func (m *Manager) GetMemoryFile(snapshotKey string) (*MemoryFile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	info, ok := m.memorySnapshots[snapshotKey]
	if !ok {
		return nil, fmt.Errorf("memory snapshot not found: %s", snapshotKey)
	}

	return &MemoryFile{
		Path:        info.Path,
		SnapshotKey: snapshotKey,
		SizeMB:      info.SizeMB,
	}, nil
}

// DeleteMemoryFile removes a memory file and its snapshot.
func (m *Manager) DeleteMemoryFile(ctx context.Context, snapshotKey string) error {
	ctx = namespaces.WithNamespace(ctx, m.namespace)

	m.mu.Lock()
	_, ok := m.memorySnapshots[snapshotKey]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("memory snapshot not found: %s", snapshotKey)
	}
	delete(m.memorySnapshots, snapshotKey)
	m.mu.Unlock()

	// Unmount
	mountPath := filepath.Join(m.dataDir, "snapshots", snapshotKey)
	if err := mount.UnmountAll(mountPath, 0); err != nil {
		log.Warn("Failed to unmount %s: %v", mountPath, err)
	}

	// Remove mount directory
	if err := os.RemoveAll(mountPath); err != nil {
		log.Warn("Failed to remove mount directory %s: %v", mountPath, err)
	}

	// Remove snapshot
	if err := m.snapshotter.Remove(ctx, snapshotKey); err != nil {
		return fmt.Errorf("failed to remove snapshot: %w", err)
	}

	return nil
}

// CreateSnapshot creates a snapshot of VM memory and state.
func (m *Manager) CreateSnapshot(ctx context.Context, vmID, snapshotID string, vmStatePath string) (string, error) {
	ctx = namespaces.WithNamespace(ctx, m.namespace)

	// Get VM's memory snapshot key
	m.mu.RLock()
	var memSnapshotKey string
	for key, info := range m.memorySnapshots {
		if info.VMID == vmID {
			memSnapshotKey = key
			break
		}
	}
	m.mu.RUnlock()

	if memSnapshotKey == "" {
		return "", fmt.Errorf("no memory file found for VM %s", vmID)
	}

	// Create new snapshot from memory snapshot
	newSnapshotKey := fmt.Sprintf("xrun-snapshot-%s-%s", vmID, snapshotID)

	labels := map[string]string{
		LabelMemorySnapshot: "true",
		LabelVMID:           vmID,
		LabelSnapshotID:     snapshotID,
		LabelCreatedAt:      time.Now().Format(time.RFC3339),
	}

	opts := snapshots.WithLabels(labels)

	// Create snapshot from parent (memory snapshot)
	if _, err := m.snapshotter.Prepare(ctx, newSnapshotKey, memSnapshotKey, opts); err != nil {
		return "", fmt.Errorf("failed to prepare snapshot: %w", err)
	}

	// Commit the snapshot
	if err := m.snapshotter.Commit(ctx, newSnapshotKey, newSnapshotKey); err != nil {
		return "", fmt.Errorf("failed to commit snapshot: %w", err)
	}

	// Store VM state file in snapshot directory
	snapshotDir := filepath.Join(m.dataDir, "snapshots", newSnapshotKey)
	stateDest := filepath.Join(snapshotDir, "vm.state")
	if err := copyFile(vmStatePath, stateDest); err != nil {
		return "", fmt.Errorf("failed to copy VM state: %w", err)
	}

	// Add to cache
	m.mu.Lock()
	m.memorySnapshots[newSnapshotKey] = &MemorySnapshotInfo{
		SnapshotKey: newSnapshotKey,
		VMID:        vmID,
		SnapshotID:  snapshotID,
		CreatedAt:   time.Now(),
		Path:        filepath.Join(snapshotDir, "memory.file"),
	}
	m.mu.Unlock()

	return newSnapshotKey, nil
}

// RestoreSnapshot restores a VM from a snapshot.
func (m *Manager) RestoreSnapshot(ctx context.Context, snapshotKey, newVMID string) (*MemoryFile, error) {
	ctx = namespaces.WithNamespace(ctx, m.namespace)

	// Get snapshot info
	m.mu.RLock()
	info, ok := m.memorySnapshots[snapshotKey]
	m.mu.RUnlock()

	if !ok {
		// Try to load from containerd
		snapInfo, err := m.snapshotter.Stat(ctx, snapshotKey)
		if err != nil {
			return nil, fmt.Errorf("snapshot not found: %s", snapshotKey)
		}
		info = &MemorySnapshotInfo{
			SnapshotKey: snapshotKey,
			VMID:        snapInfo.Labels[LabelVMID],
			SnapshotID:  snapInfo.Labels[LabelSnapshotID],
		}
	}

	// Create new memory snapshot from the snapshot
	newSnapshotKey := fmt.Sprintf("xrun-memory-%s-%s", newVMID, uuid.New().String()[:8])

	labels := map[string]string{
		LabelMemorySnapshot: "true",
		LabelVMID:           newVMID,
		LabelCreatedAt:      time.Now().Format(time.RFC3339),
	}

	opts := snapshots.WithLabels(labels)

	mounts, err := m.snapshotter.Prepare(ctx, newSnapshotKey, snapshotKey, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare restore snapshot: %w", err)
	}

	// Mount the snapshot
	mountPath := filepath.Join(m.dataDir, "snapshots", newSnapshotKey)
	if err := os.MkdirAll(mountPath, 0755); err != nil {
		m.snapshotter.Remove(ctx, newSnapshotKey)
		return nil, fmt.Errorf("failed to create mount directory: %w", err)
	}

	if err := mount.All(mounts, mountPath); err != nil {
		os.RemoveAll(mountPath)
		m.snapshotter.Remove(ctx, newSnapshotKey)
		return nil, fmt.Errorf("failed to mount snapshot: %w", err)
	}

	// Commit
	if err := m.snapshotter.Commit(ctx, newSnapshotKey, newSnapshotKey); err != nil {
		log.Warn("Failed to commit snapshot: %v", err)
	}

	memFilePath := filepath.Join(mountPath, "memory.file")

	// Add to cache
	m.mu.Lock()
	m.memorySnapshots[newSnapshotKey] = &MemorySnapshotInfo{
		SnapshotKey: newSnapshotKey,
		VMID:        newVMID,
		SnapshotID:  info.SnapshotID,
		CreatedAt:   time.Now(),
		Path:        memFilePath,
	}
	m.mu.Unlock()

	return &MemoryFile{
		Path:        memFilePath,
		SnapshotKey: newSnapshotKey,
		SizeMB:      info.SizeMB,
	}, nil
}

// GetVMStatePath returns the path to VM state file in a snapshot.
func (m *Manager) GetVMStatePath(snapshotKey string) string {
	return filepath.Join(m.dataDir, "snapshots", snapshotKey, "vm.state")
}

// createSparseFile creates a sparse file of the given size.
func createSparseFile(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := f.Truncate(size); err != nil {
		return err
	}

	return nil
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}
