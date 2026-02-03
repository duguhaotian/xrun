// Package config provides configuration for xrund daemon.
package config

import (
	"os"
	"path/filepath"
)

// Config holds all configuration for the daemon.
type Config struct {
	// Daemon settings
	SocketPath string // Unix socket path for gRPC
	DataDir    string // Data directory for sandboxes
	LogLevel   string // debug, info, warn, error

	// Containerd settings
	ContainerdAddress string
	SnapshotNamespace string // default: "default"
	Snapshotter       string // default: "overlayfs"

	// Network settings
	Network NetworkConfig

	// VMM settings
	VMM VMMConfig
}

// NetworkConfig holds network configuration.
type NetworkConfig struct {
	Enabled       bool   // Network feature toggle, default: false
	UsePool       bool   // Use netns pool, default: false
	PoolSize      int    // Initial pool size, default: 10
	CNIPluginPath string // Path to CNI binary
	CNIConfigPath string // Path to CNI config
	VMSubnet      string // VM subnet, e.g., "10.0.0.0/24"
	VMGateway     string // VM gateway, e.g., "10.0.0.1"
	VMIPRange     string // VM IP range, e.g., "10.0.0.10-10.0.0.250"
}

// VMMConfig holds VMM configuration.
type VMMConfig struct {
	DefaultDriver string // default: "cloud-hypervisor"
	CHBinaryPath  string // default: "cloud-hypervisor"
}

// DefaultConfig returns default configuration.
func DefaultConfig() *Config {
	return &Config{
		SocketPath:        "/run/xrun/xrund.sock",
		DataDir:           "/var/lib/xrun",
		LogLevel:          "info",
		ContainerdAddress: "/run/containerd/containerd.sock",
		SnapshotNamespace: "default",
		Snapshotter:       "overlayfs",
		Network: NetworkConfig{
			Enabled:  false,
			UsePool:  false,
			PoolSize: 10,
		},
		VMM: VMMConfig{
			DefaultDriver: "cloud-hypervisor",
			CHBinaryPath:  "cloud-hypervisor",
		},
	}
}

// LoadFromEnv loads configuration from environment variables.
func (c *Config) LoadFromEnv() {
	if v := os.Getenv("XRUN_SOCKET"); v != "" {
		c.SocketPath = v
	}
	if v := os.Getenv("XRUN_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("XRUN_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("XRUN_CONTAINERD_ADDRESS"); v != "" {
		c.ContainerdAddress = v
	}
	if v := os.Getenv("XRUN_SNAPSHOT_NAMESPACE"); v != "" {
		c.SnapshotNamespace = v
	}
}

// EnsureDirs creates necessary directories.
func (c *Config) EnsureDirs() error {
	dirs := []string{
		c.DataDir,
		filepath.Join(c.DataDir, "sandboxes"),
		filepath.Join(c.DataDir, "images"),
		filepath.Join(c.DataDir, "snapshots"),
		filepath.Join(c.DataDir, "logs"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	// Ensure socket directory exists
	socketDir := filepath.Dir(c.SocketPath)
	if err := os.MkdirAll(socketDir, 0755); err != nil {
		return err
	}

	return nil
}
