// Package image provides OCI image and containerd snapshot integration for microVM sandboxes.
package image

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/snapshots"
)

// Manager handles OCI image operations and containerd snapshots.
type Manager struct {
	client      *containerd.Client
	namespace   string
	snapshotter string
}

// Config contains image manager configuration.
type Config struct {
	Address     string // containerd socket address
	Namespace   string // containerd namespace
	Snapshotter string // snapshotter name (e.g., "overlayfs")
}

// NewManager creates a new image manager.
func NewManager(config Config) (*Manager, error) {
	if config.Address == "" {
		config.Address = "/run/containerd/containerd.sock"
	}
	if config.Namespace == "" {
		config.Namespace = "microvm-sandbox"
	}
	if config.Snapshotter == "" {
		config.Snapshotter = "overlayfs"
	}

	client, err := containerd.New(config.Address)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to containerd: %w", err)
	}

	return &Manager{
		client:      client,
		namespace:   config.Namespace,
		snapshotter: config.Snapshotter,
	}, nil
}

// Close closes the containerd client connection.
func (m *Manager) Close() error {
	if m.client != nil {
		return m.client.Close()
	}
	return nil
}

// KernelImage represents a kernel OCI image containing kernel and initrd.
type KernelImage struct {
	ImageRef    string
	KernelPath  string // Relative path in the image
	InitrdPath  string // Relative path in the image
	SnapshotKey string
	MountPath   string
}

// PullAndPrepareKernel pulls a kernel OCI image and prepares it via snapshotter.
func (m *Manager) PullAndPrepareKernel(ctx context.Context, imageRef string) (*KernelImage, error) {
	ctx = namespaces.WithNamespace(ctx, m.namespace)

	// Pull the image
	image, err := m.client.Pull(ctx, imageRef, containerd.WithPullUnpack)
	if err != nil {
		return nil, fmt.Errorf("failed to pull image %s: %w", imageRef, err)
	}

	// Get image config to find kernel and initrd paths
	// For kernel images, we expect standard paths like:
	// - /boot/vmlinuz or /boot/bzImage (kernel)
	// - /boot/initrd.img or /boot/initramfs (initrd)
	kernelImg := &KernelImage{
		ImageRef:    imageRef,
		KernelPath:  "boot/vmlinuz",
		InitrdPath:  "boot/initrd.img",
		SnapshotKey: fmt.Sprintf("kernel-%s", generateID()),
	}

	// Create a snapshot from the image
	snapshotter := m.client.SnapshotService(m.snapshotter)

	// Get the image's rootfs
	desc, err := image.Config(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get image config: %w", err)
	}

	// Create snapshot
	mounts, err := snapshotter.Prepare(ctx, kernelImg.SnapshotKey, desc.Digest.String())
	if err != nil {
		return nil, fmt.Errorf("failed to prepare snapshot: %w", err)
	}

	// Mount the snapshot to a temporary directory
	mountPath, err := os.MkdirTemp("", "kernel-snapshot-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create mount directory: %w", err)
	}
	kernelImg.MountPath = mountPath

	// Perform the mount
	if err := mount.All(mounts, mountPath); err != nil {
		os.RemoveAll(mountPath)
		return nil, fmt.Errorf("failed to mount snapshot: %w", err)
	}

	// Verify kernel and initrd exist
	kernelFullPath := filepath.Join(mountPath, kernelImg.KernelPath)
	initrdFullPath := filepath.Join(mountPath, kernelImg.InitrdPath)

	if _, err := os.Stat(kernelFullPath); os.IsNotExist(err) {
		// Try alternative paths
		altKernelPath := filepath.Join(mountPath, "boot/bzImage")
		if _, err := os.Stat(altKernelPath); err == nil {
			kernelImg.KernelPath = "boot/bzImage"
		} else {
			mount.UnmountAll(mountPath, 0)
			os.RemoveAll(mountPath)
			return nil, fmt.Errorf("kernel not found in image at expected paths")
		}
	}

	if _, err := os.Stat(initrdFullPath); os.IsNotExist(err) {
		// Try alternative paths
		altInitrdPath := filepath.Join(mountPath, "boot/initramfs")
		if _, err := os.Stat(altInitrdPath); err == nil {
			kernelImg.InitrdPath = "boot/initramfs"
		}
		// Initrd is optional for some setups
	}

	return kernelImg, nil
}

// GetKernelPaths returns the full paths to kernel and initrd in the mounted snapshot.
func (k *KernelImage) GetKernelPaths() (kernelPath, initrdPath string) {
	kernelPath = filepath.Join(k.MountPath, k.KernelPath)
	if k.InitrdPath != "" {
		initrdPath = filepath.Join(k.MountPath, k.InitrdPath)
	}
	return
}

// Cleanup unmounts and removes the snapshot.
func (k *KernelImage) Cleanup(ctx context.Context, snapshotter snapshots.Snapshotter) error {
	// Unmount
	if err := mount.UnmountAll(k.MountPath, 0); err != nil {
		return fmt.Errorf("failed to unmount: %w", err)
	}

	// Remove mount directory
	if err := os.RemoveAll(k.MountPath); err != nil {
		return fmt.Errorf("failed to remove mount directory: %w", err)
	}

	// Remove snapshot
	if err := snapshotter.Remove(ctx, k.SnapshotKey); err != nil {
		return fmt.Errorf("failed to remove snapshot: %w", err)
	}

	return nil
}

// Snapshotter returns the snapshotter service.
func (m *Manager) Snapshotter() snapshots.Snapshotter {
	return m.client.SnapshotService(m.snapshotter)
}

// generateID generates a unique identifier.
func generateID() string {
	// Simple implementation - in production, use proper UUID
	return fmt.Sprintf("%d", os.Getpid())
}
