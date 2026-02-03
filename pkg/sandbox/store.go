package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/microvm/sandbox/pkg/vmm"
)

// SandboxMeta contains metadata for a sandbox.
type SandboxMeta struct {
	ID          string      `json:"id"`
	VMM         string      `json:"vmm"`
	VCPUs       uint32      `json:"vcpus"`
	MemoryMB    uint32      `json:"memory_mb"`
	Image       string      `json:"image,omitempty"`
	RootFS      string      `json:"rootfs,omitempty"`
	Cmdline     string      `json:"cmdline"`
	State       vmm.VMState `json:"state"`
	SnapshotKey string      `json:"snapshot_key,omitempty"`
	MountPath   string      `json:"mount_path,omitempty"`
	Namespace   string      `json:"namespace,omitempty"`
	CreatedAt   string      `json:"created_at"`
}

// Store handles persistence of sandbox metadata.
type Store struct {
	dataDir string
	mu      sync.RWMutex
}

// NewStore creates a new metadata store.
func NewStore(dataDir string) *Store {
	return &Store{
		dataDir: dataDir,
	}
}

// Save persists sandbox metadata to disk.
func (s *Store) Save(meta *SandboxMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Ensure directory exists
	sandboxDir := filepath.Join(s.dataDir, "sandboxes", meta.ID)
	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		return fmt.Errorf("failed to create sandbox directory: %w", err)
	}

	// Write metadata file
	metaPath := filepath.Join(sandboxDir, "meta.json")
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	if err := os.WriteFile(metaPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write metadata file: %w", err)
	}

	return nil
}

// Load retrieves sandbox metadata from disk.
func (s *Store) Load(id string) (*SandboxMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	metaPath := filepath.Join(s.dataDir, "sandboxes", id, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("sandbox %s not found", id)
		}
		return nil, fmt.Errorf("failed to read metadata file: %w", err)
	}

	var meta SandboxMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
	}

	return &meta, nil
}

// List returns all stored sandbox metadata.
func (s *Store) List() ([]*SandboxMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sandboxesDir := filepath.Join(s.dataDir, "sandboxes")
	entries, err := os.ReadDir(sandboxesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []*SandboxMeta{}, nil
		}
		return nil, fmt.Errorf("failed to read sandboxes directory: %w", err)
	}

	var metas []*SandboxMeta
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		metaPath := filepath.Join(sandboxesDir, entry.Name(), "meta.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue // Skip invalid entries
		}

		var meta SandboxMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			continue // Skip invalid entries
		}

		metas = append(metas, &meta)
	}

	return metas, nil
}

// UpdateState updates the sandbox state in storage.
func (s *Store) UpdateState(id string, state vmm.VMState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Load metadata directly without calling Load() to avoid lock reentrancy
	metaPath := filepath.Join(s.dataDir, "sandboxes", id, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("sandbox %s not found", id)
		}
		return fmt.Errorf("failed to read metadata file: %w", err)
	}

	var meta SandboxMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("failed to unmarshal metadata: %w", err)
	}

	meta.State = state

	// Save directly without calling Save() to avoid lock reentrancy
	sandboxDir := filepath.Join(s.dataDir, "sandboxes", meta.ID)
	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		return fmt.Errorf("failed to create sandbox directory: %w", err)
	}

	data, err = json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	if err := os.WriteFile(metaPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write metadata file: %w", err)
	}

	return nil
}

// Delete removes sandbox metadata from disk.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandboxDir := filepath.Join(s.dataDir, "sandboxes", id)
	if err := os.RemoveAll(sandboxDir); err != nil {
		return fmt.Errorf("failed to remove sandbox directory: %w", err)
	}

	return nil
}
