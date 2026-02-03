// Package network provides network management for microVM sandboxes.
package network

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/microvm/sandbox/pkg/log"
)

// Config holds network configuration.
type Config struct {
	Enabled       bool
	UsePool       bool
	PoolSize      int
	CNIPluginPath string
	CNIConfigPath string
	VMSubnet      string
	VMGateway     string
	VMIPRange     string
}

// Manager handles network setup for sandboxes.
type Manager struct {
	config  Config
	pool    *NetNSPool
	ipam    *IPAM
	baseDir string
}

// NetNS represents a network namespace.
type NetNS struct {
	Name  string
	Path  string
	InUse bool
}

// VMNetwork holds network configuration for a VM.
type VMNetwork struct {
	NetNS     *NetNS
	TapDevice string
	VMIP      string
	HostIP    string
	Gateway   string
}

// NewManager creates a new network manager.
func NewManager(config Config, baseDir string) *Manager {
	return &Manager{
		config:  config,
		baseDir: filepath.Join(baseDir, "netns"),
		ipam:    NewIPAM(config.VMSubnet, config.VMIPRange),
	}
}

// Init initializes the network manager.
func (m *Manager) Init() error {
	if !m.config.Enabled {
		return nil
	}

	// Create netns directory
	if err := os.MkdirAll(m.baseDir, 0755); err != nil {
		return fmt.Errorf("failed to create netns directory: %w", err)
	}

	// Initialize pool if enabled
	if m.config.UsePool {
		m.pool = NewNetNSPool(m.baseDir, m.config.PoolSize)
		if err := m.pool.Init(); err != nil {
			return fmt.Errorf("failed to initialize netns pool: %w", err)
		}
	}

	return nil
}

// Setup sets up network for a VM.
func (m *Manager) Setup(ctx context.Context, vmID string) (*VMNetwork, error) {
	if !m.config.Enabled {
		return &VMNetwork{}, nil
	}

	// Get or create netns
	var ns *NetNS
	var err error
	if m.config.UsePool {
		ns, err = m.pool.Acquire()
	} else {
		ns, err = m.createNetNS(vmID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get netns: %w", err)
	}

	// Call CNI to create veth pair
	hostIP, err := m.callCNI(ctx, "ADD", ns.Name, vmID)
	if err != nil {
		m.releaseNetNS(ns)
		return nil, fmt.Errorf("failed to setup CNI: %w", err)
	}

	// Allocate VM IP
	vmIP := m.ipam.Allocate(vmID)

	// Create tap device in netns
	tapName := fmt.Sprintf("tap-%s", vmID[:8])
	if err := m.createTapInNS(ns.Path, tapName); err != nil {
		m.callCNI(ctx, "DEL", ns.Name, vmID)
		m.releaseNetNS(ns)
		return nil, fmt.Errorf("failed to create tap: %w", err)
	}

	return &VMNetwork{
		NetNS:     ns,
		TapDevice: tapName,
		VMIP:      vmIP,
		HostIP:    hostIP,
		Gateway:   m.config.VMGateway,
	}, nil
}

// Cleanup cleans up network for a VM.
func (m *Manager) Cleanup(ctx context.Context, vmID string, netns *NetNS) error {
	if !m.config.Enabled || netns == nil {
		return nil
	}

	// Call CNI to delete veth
	if _, err := m.callCNI(ctx, "DEL", netns.Name, vmID); err != nil {
		log.Warn("Failed to cleanup CNI for %s: %v", vmID, err)
	}

	// Release IP
	m.ipam.Release(vmID)

	// Release netns
	m.releaseNetNS(netns)

	return nil
}

// createNetNS creates a new network namespace.
func (m *Manager) createNetNS(vmID string) (*NetNS, error) {
	name := fmt.Sprintf("xrun-%s", vmID)
	path := filepath.Join(m.baseDir, name)

	// Create using ip netns
	cmd := exec.Command("ip", "netns", "add", name)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to create netns: %w", err)
	}

	// Get actual path
	path = filepath.Join("/var/run/netns", name)

	return &NetNS{
		Name:  name,
		Path:  path,
		InUse: true,
	}, nil
}

// releaseNetNS releases a network namespace.
func (m *Manager) releaseNetNS(ns *NetNS) {
	if m.config.UsePool {
		m.pool.Release(ns)
	} else {
		// Delete the netns
		exec.Command("ip", "netns", "del", ns.Name).Run()
	}
}

// callCNI calls the CNI plugin.
func (m *Manager) callCNI(ctx context.Context, command, netns, containerID string) (string, error) {
	// Read CNI config
	config, err := os.ReadFile(m.config.CNIConfigPath)
	if err != nil {
		return "", fmt.Errorf("failed to read CNI config: %w", err)
	}

	// Set environment variables
	cmd := exec.CommandContext(ctx, m.config.CNIPluginPath)
	cmd.Env = []string{
		fmt.Sprintf("CNI_COMMAND=%s", command),
		fmt.Sprintf("CNI_CONTAINERID=%s", containerID),
		fmt.Sprintf("CNI_NETNS=%s", netns),
		fmt.Sprintf("CNI_IFNAME=eth0"),
		fmt.Sprintf("CNI_PATH=%s", filepath.Dir(m.config.CNIPluginPath)),
	}
	cmd.Stdin = strings.NewReader(string(config))

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("CNI failed: %s", string(exitErr.Stderr))
		}
		return "", fmt.Errorf("failed to execute CNI: %w", err)
	}

	// Parse result to get IP (simplified)
	// In production, parse the JSON result properly
	return m.extractIP(string(output)), nil
}

// createTapInNS creates a tap device in the specified netns.
func (m *Manager) createTapInNS(netnsPath, tapName string) error {
	// Use ip command to create tap in netns
	cmd := exec.Command("ip", "netns", "exec", filepath.Base(netnsPath),
		"ip", "tuntap", "add", tapName, "mode", "tap")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create tap: %w", err)
	}

	// Bring up the tap
	cmd = exec.Command("ip", "netns", "exec", filepath.Base(netnsPath),
		"ip", "link", "set", tapName, "up")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to bring up tap: %w", err)
	}

	return nil
}

// extractIP extracts IP from CNI result (simplified).
func (m *Manager) extractIP(output string) string {
	// TODO: Properly parse CNI result JSON
	// For now, return a placeholder
	return ""
}

// NetNSPool manages a pool of network namespaces.
type NetNSPool struct {
	baseDir   string
	minSize   int
	currentID int
	pool      chan *NetNS
	mu        sync.Mutex
}

// NewNetNSPool creates a new netns pool.
func NewNetNSPool(baseDir string, minSize int) *NetNSPool {
	return &NetNSPool{
		baseDir: baseDir,
		minSize: minSize,
		pool:    make(chan *NetNS, minSize*2), // Buffer for expansion
	}
}

// Init initializes the pool.
func (p *NetNSPool) Init() error {
	for i := 0; i < p.minSize; i++ {
		ns, err := p.createNetNS(i)
		if err != nil {
			return err
		}
		p.pool <- ns
	}
	p.currentID = p.minSize - 1
	return nil
}

// Acquire gets a netns from the pool (expands if empty).
func (p *NetNSPool) Acquire() (*NetNS, error) {
	select {
	case ns := <-p.pool:
		ns.InUse = true
		return ns, nil
	default:
		// Pool empty, expand
		return p.expandAndAcquire()
	}
}

// Release returns a netns to the pool.
func (p *NetNSPool) Release(ns *NetNS) {
	ns.InUse = false
	p.pool <- ns
}

// createNetNS creates a new netns with the given ID.
func (p *NetNSPool) createNetNS(id int) (*NetNS, error) {
	name := fmt.Sprintf("xrun-pool-%d", id)

	cmd := exec.Command("ip", "netns", "add", name)
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	return &NetNS{
		Name: name,
		Path: filepath.Join("/var/run/netns", name),
	}, nil
}

// expandAndAcquire creates a new netns when pool is empty.
func (p *NetNSPool) expandAndAcquire() (*NetNS, error) {
	p.mu.Lock()
	p.currentID++
	id := p.currentID
	p.mu.Unlock()

	ns, err := p.createNetNS(id)
	if err != nil {
		return nil, err
	}
	ns.InUse = true
	return ns, nil
}

// IPAM manages IP address allocation.
type IPAM struct {
	subnet    string
	ipRange   string
	allocated map[string]string // vmID -> IP
	mu        sync.Mutex
}

// NewIPAM creates a new IPAM.
func NewIPAM(subnet, ipRange string) *IPAM {
	return &IPAM{
		subnet:    subnet,
		ipRange:   ipRange,
		allocated: make(map[string]string),
	}
}

// Allocate allocates an IP for a VM.
func (i *IPAM) Allocate(vmID string) string {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Check if already allocated
	if ip, ok := i.allocated[vmID]; ok {
		return ip
	}

	// TODO: Implement proper IP allocation from range
	// For now, use a simple counter
	ip := fmt.Sprintf("10.0.0.%d", 10+len(i.allocated))
	i.allocated[vmID] = ip
	return ip
}

// Release releases an IP.
func (i *IPAM) Release(vmID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.allocated, vmID)
}
