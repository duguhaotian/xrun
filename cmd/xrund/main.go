// xrund is the daemon for managing microVM sandboxes.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/microvm/sandbox/internal/config"
	"github.com/microvm/sandbox/pkg/daemon"
	"github.com/microvm/sandbox/pkg/log"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Parse flags
	var (
		socketPath        = flag.String("socket", "/run/xrun/xrund.sock", "Unix socket path")
		dataDir           = flag.String("data-dir", "/var/lib/xrun", "Data directory")
		logLevel          = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
		containerdAddr    = flag.String("containerd-address", "/run/containerd/containerd.sock", "Containerd socket path")
		snapshotNamespace = flag.String("snapshot-namespace", "default", "Containerd snapshot namespace")
		snapshotter       = flag.String("snapshotter", "overlayfs", "Snapshotter name")
		chBinaryPath      = flag.String("ch-binary", "cloud-hypervisor", "Cloud-hypervisor binary path")
	)
	flag.Parse()

	// Create config
	cfg := &config.Config{
		SocketPath:        *socketPath,
		DataDir:           *dataDir,
		LogLevel:          *logLevel,
		ContainerdAddress: *containerdAddr,
		SnapshotNamespace: *snapshotNamespace,
		Snapshotter:       *snapshotter,
		VMM: config.VMMConfig{
			DefaultDriver: "cloud-hypervisor",
			CHBinaryPath:  *chBinaryPath,
		},
	}

	// Load from environment
	cfg.LoadFromEnv()

	// Ensure directories
	if err := cfg.EnsureDirs(); err != nil {
		return fmt.Errorf("failed to create directories: %w", err)
	}

	// Initialize logging
	if err := log.Init(cfg.DataDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to initialize logging: %v\n", err)
	}

	log.Info("Starting xrund...")
	log.Info("Socket: %s", cfg.SocketPath)
	log.Info("DataDir: %s", cfg.DataDir)

	// Create server
	server, err := daemon.NewServer(cfg)
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	// Initialize
	ctx := context.Background()
	if err := server.Init(ctx); err != nil {
		return fmt.Errorf("failed to initialize server: %w", err)
	}

	// Start server
	if err := server.Start(); err != nil {
		return fmt.Errorf("failed to start server: %w", err)
	}

	return nil
}
