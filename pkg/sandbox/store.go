// Package sandbox provides high-level sandbox management.
package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/microvm/sandbox/pkg/vmm"
)

// NetworkInfo holds network-related metadata for a sandbox.
type NetworkInfo struct {
	NetNS     string `json:"netns,omitempty"`      // Network namespace path
	TapDevice string `json:"tap_device,omitempty"` // TAP device name
	IPAddr    string `json:"ip_addr,omitempty"`    // IP address
	Gateway   string `json:"gateway,omitempty"`    // Gateway IP
	Enabled   bool   `json:"enabled"`              // Whether network is enabled
}

// ImageInfo holds image-related metadata for a sandbox.
type ImageInfo struct {
	Image      string `json:"image"`                 // Image reference
	ImageMount string `json:"image_mount,omitempty"` // Path to mounted image
	RootFS     string `json:"rootfs,omitempty"`      // Root filesystem
	KernelPath string `json:"kernel_path,omitempty"` // Kernel path
	InitrdPath string `json:"initrd_path,omitempty"` // Initrd path
}

// Meta contains metadata for a sandbox.
type Meta struct {
	ID             string            `json:"id"`
	VMM            string            `json:"vmm"`
	VCPUs          uint32            `json:"vcpus"`
	MemoryMB       uint32            `json:"memory_mb"`
	Cmdline        string            `json:"cmdline"`
	State          vmm.VMState       `json:"state"`
	PID            int               `json:"pid,omitempty"`
	MemorySnapshot string            `json:"memory_snapshot,omitempty"` // Key for memory file snapshot
	CreatedAt      string            `json:"created_at"`
	Labels         map[string]string `json:"labels,omitempty"`

	// Sub-structs for better organization and nil-safety
	Network *NetworkInfo `json:"network,omitempty"`
	Image   *ImageInfo   `json:"image,omitempty"`
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
func (s *Store) Save(meta *Meta) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandboxDir := filepath.Join(s.dataDir, "sandboxes", meta.ID)
	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		return fmt.Errorf("failed to create sandbox directory: %w", err)
	}

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
func (s *Store) Load(id string) (*Meta, error) {
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

	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
	}

	return &meta, nil
}

// List returns all stored sandbox metadata.
func (s *Store) List() ([]*Meta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sandboxesDir := filepath.Join(s.dataDir, "sandboxes")
	entries, err := os.ReadDir(sandboxesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []*Meta{}, nil
		}
		return nil, fmt.Errorf("failed to read sandboxes directory: %w", err)
	}

	var metas []*Meta
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		metaPath := filepath.Join(sandboxesDir, entry.Name(), "meta.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue // Skip invalid entries
		}

		var meta Meta
		if err := json.Unmarshal(data, &meta); err != nil {
			continue // Skip invalid entries
		}

		metas = append(metas, &meta)
	}

	return metas, nil
}

// UpdateState updates the sandbox state.
func (s *Store) UpdateState(id string, state vmm.VMState) error {
	meta, err := s.Load(id)
	if err != nil {
		return err
	}

	meta.State = state
	return s.Save(meta)
}

// UpdatePID updates the sandbox PID.
func (s *Store) UpdatePID(id string, pid int) error {
	meta, err := s.Load(id)
	if err != nil {
		return err
	}

	meta.PID = pid
	return s.Save(meta)
}

// Delete removes sandbox metadata.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandboxDir := filepath.Join(s.dataDir, "sandboxes", id)
	if err := os.RemoveAll(sandboxDir); err != nil {
		return fmt.Errorf("failed to remove sandbox directory: %w", err)
	}

	return nil
}

// CreateMeta creates a new Meta with default values.
func CreateMeta(id, vmmType string, vcpus, memoryMB uint32) *Meta {
	return &Meta{
		ID:        id,
		VMM:       vmmType,
		VCPUs:     vcpus,
		MemoryMB:  memoryMB,
		State:     "pending",
		CreatedAt: time.Now().Format(time.RFC3339),
		Labels:    make(map[string]string),
	}
}

// GetNetworkInfo returns network info, creating if nil.
func (m *Meta) GetNetworkInfo() *NetworkInfo {
	if m.Network == nil {
		m.Network = &NetworkInfo{}
	}
	return m.Network
}

// GetImageInfo returns image info, creating if nil.
func (m *Meta) GetImageInfo() *ImageInfo {
	if m.Image == nil {
		m.Image = &ImageInfo{}
	}
	return m.Image
}

// SetNetworkInfo sets network info from VMNetwork.
func (m *Meta) SetNetworkInfo(netNSPath, tapDevice, ipAddr string, enabled bool) {
	m.Network = &NetworkInfo{
		NetNS:     netNSPath,
		TapDevice: tapDevice,
		IPAddr:    ipAddr,
		Enabled:   enabled,
	}
}

// SetImageInfo sets image info from image mount.
func (m *Meta) SetImageInfo(image, mountPath, rootFS, kernelPath, initrdPath string) {
	m.Image = &ImageInfo{
		Image:      image,
		ImageMount: mountPath,
		RootFS:     rootFS,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
	}
}

// GetNetNSPath returns the network namespace path or empty string if nil.
func (m *Meta) GetNetNSPath() string {
	if m.Network == nil {
		return ""
	}
	return m.Network.NetNS
}

// GetTapDevice returns the TAP device name or empty string if nil.
func (m *Meta) GetTapDevice() string {
	if m.Network == nil {
		return ""
	}
	return m.Network.TapDevice
}

// GetIPAddr returns the IP address or empty string if nil.
func (m *Meta) GetIPAddr() string {
	if m.Network == nil {
		return ""
	}
	return m.Network.IPAddr
}

// GetImageMount returns the image mount path or empty string if nil.
func (m *Meta) GetImageMount() string {
	if m.Image == nil {
		return ""
	}
	return m.Image.ImageMount
}

// GetImageRef returns the image reference or empty string if nil.
func (m *Meta) GetImageRef() string {
	if m.Image == nil {
		return ""
	}
	return m.Image.Image
}
