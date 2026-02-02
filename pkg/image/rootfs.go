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

// RootFSManager handles creating and managing rootfs from OCI images.
type RootFSManager struct {
	client      *Client
	snapshotter snapshots.Snapshotter
	dataDir     string
}

// RootFS represents a prepared rootfs with kernel and initrd.
type RootFS struct {
	KernelPath  string
	InitrdPath  string
	SnapshotKey string
	MountPath   string
	ImageRef    string
}

// NewRootFSManager creates a new rootfs manager.
func NewRootFSManager(client *Client, dataDir string) *RootFSManager {
	return &RootFSManager{
		client:      client,
		snapshotter: client.Snapshotter(),
		dataDir:     dataDir,
	}
}

// PrepareRootFS pulls an image (if not exists), creates a snapshot, and extracts kernel/initrd paths.
func (m *RootFSManager) PrepareRootFS(ctx context.Context, imageRef, sandboxID string) (*RootFS, error) {
	ctx = namespaces.WithNamespace(ctx, m.client.namespace)

	// Try to get image from local first
	img, err := m.client.GetImage(ctx, imageRef)
	if err != nil {
		// Image not found locally, pull it
		img, err = m.client.PullImage(ctx, imageRef)
		if err != nil {
			return nil, fmt.Errorf("failed to pull image %s: %w", imageRef, err)
		}
	}

	// Unpack the image to snapshotter
	if err := img.Unpack(ctx, m.client.snapshotter); err != nil {
		return nil, fmt.Errorf("failed to unpack image: %w", err)
	}

	// Get image rootfs (unpacked diffids)
	rootfs, err := img.RootFS(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get image rootfs: %w", err)
	}

	if len(rootfs) == 0 {
		return nil, fmt.Errorf("image has no rootfs layers")
	}

	// Use the last rootfs layer as the base for our snapshot
	// This represents the complete filesystem
	parentDigest := rootfs[len(rootfs)-1]

	// Generate unique snapshot key for this sandbox
	snapshotKey := fmt.Sprintf("sandbox-%s-rootfs", sandboxID)

	// Create mount directory specific to this sandbox
	mountPath := filepath.Join(m.dataDir, "sandboxes", sandboxID, "rootfs")
	if err := os.MkdirAll(mountPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create mount directory: %w", err)
	}

	// Prepare snapshot - this creates a new snapshot based on the image layer
	mounts, err := m.snapshotter.Prepare(ctx, snapshotKey, parentDigest.String())
	if err != nil {
		os.RemoveAll(mountPath)
		return nil, fmt.Errorf("failed to prepare snapshot: %w", err)
	}

	// Mount the snapshot
	if err := mount.All(mounts, mountPath); err != nil {
		m.snapshotter.Remove(ctx, snapshotKey)
		os.RemoveAll(mountPath)
		return nil, fmt.Errorf("failed to mount snapshot: %w", err)
	}

	// Find kernel and initrd in the mounted rootfs
	kernelPath, initrdPath, err := m.findKernelFiles(mountPath)
	if err != nil {
		mount.UnmountAll(mountPath, 0)
		m.snapshotter.Remove(ctx, snapshotKey)
		os.RemoveAll(mountPath)
		return nil, err
	}

	return &RootFS{
		KernelPath:  kernelPath,
		InitrdPath:  initrdPath,
		SnapshotKey: snapshotKey,
		MountPath:   mountPath,
		ImageRef:    imageRef,
	}, nil
}

// findKernelFiles searches for kernel and initrd in the mounted rootfs.
func (m *RootFSManager) findKernelFiles(mountPath string) (kernelPath, initrdPath string, err error) {
	// Common kernel file names and paths
	kernelPaths := []string{
		"boot/vmlinuz",
		"boot/bzImage",
		"boot/kernel",
		"vmlinuz",
		"bzImage",
		"kernel",
	}

	// Common initrd file names and paths
	initrdPaths := []string{
		"boot/initrd.img",
		"boot/initramfs",
		"boot/initramfs.img",
		"initrd.img",
		"initramfs",
		"initramfs.img",
	}

	// Search for kernel
	for _, relPath := range kernelPaths {
		fullPath := filepath.Join(mountPath, relPath)
		if info, err := os.Stat(fullPath); err == nil && !info.IsDir() {
			kernelPath = fullPath
			break
		}
	}

	if kernelPath == "" {
		return "", "", fmt.Errorf("kernel not found in image (searched paths: %v)", kernelPaths)
	}

	// Search for initrd (optional)
	for _, relPath := range initrdPaths {
		fullPath := filepath.Join(mountPath, relPath)
		if info, err := os.Stat(fullPath); err == nil && !info.IsDir() {
			initrdPath = fullPath
			break
		}
	}

	return kernelPath, initrdPath, nil
}

// Cleanup unmounts and removes the rootfs snapshot.
func (r *RootFS) Cleanup(ctx context.Context, snapshotter snapshots.Snapshotter) error {
	// Unmount
	if err := mount.UnmountAll(r.MountPath, 0); err != nil {
		fmt.Printf("Warning: failed to unmount %s: %v\n", r.MountPath, err)
	}

	// Remove mount directory
	if err := os.RemoveAll(r.MountPath); err != nil {
		fmt.Printf("Warning: failed to remove mount directory %s: %v\n", r.MountPath, err)
	}

	// Remove snapshot
	if err := snapshotter.Remove(ctx, r.SnapshotKey); err != nil {
		return fmt.Errorf("failed to remove snapshot %s: %w", r.SnapshotKey, err)
	}

	return nil
}

// GetImage returns the image interface for additional operations.
func (m *RootFSManager) GetImage(ctx context.Context, imageRef string) (containerd.Image, error) {
	ctx = namespaces.WithNamespace(ctx, m.client.namespace)
	return m.client.GetImage(ctx, imageRef)
}

// Snapshotter returns the snapshotter service for cleanup operations.
func (m *RootFSManager) Snapshotter() snapshots.Snapshotter {
	return m.snapshotter
}
