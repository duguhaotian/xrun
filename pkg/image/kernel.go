package image

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/snapshots"
)

// KernelResolver handles resolving kernel and initrd from OCI images.
type KernelResolver struct {
	client      *Client
	snapshotter snapshots.Snapshotter
}

// KernelBundle contains resolved kernel and initrd paths.
type KernelBundle struct {
	KernelPath  string
	InitrdPath  string
	SnapshotKey string
	MountPath   string
	imageRef    string
}

// NewKernelResolver creates a new kernel resolver.
func NewKernelResolver(client *Client) *KernelResolver {
	return &KernelResolver{
		client:      client,
		snapshotter: client.Snapshotter(),
	}
}

// Resolve pulls a kernel image and prepares it for use.
func (r *KernelResolver) Resolve(ctx context.Context, imageRef string) (*KernelBundle, error) {
	ctx = namespaces.WithNamespace(ctx, r.client.namespace)

	// Pull the image
	img, err := r.client.PullImage(ctx, imageRef)
	if err != nil {
		return nil, fmt.Errorf("failed to pull kernel image %s: %w", imageRef, err)
	}

	// Get image rootfs (unpacked diffids)
	rootfs, err := img.RootFS(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get image rootfs: %w", err)
	}

	if len(rootfs) == 0 {
		return nil, fmt.Errorf("image has no rootfs layers")
	}

	// Use the first rootfs layer's digest as the parent for snapshot
	layerDigest := rootfs[0]

	// Generate unique snapshot key
	snapshotKey := fmt.Sprintf("kernel-snapshot-%s-%d", sanitizeRef(imageRef), os.Getpid())

	// Create mount directory
	mountPath, err := os.MkdirTemp("", "kernel-snapshot-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create mount directory: %w", err)
	}

	// Prepare snapshot
	mounts, err := r.snapshotter.Prepare(ctx, snapshotKey, layerDigest.String())
	if err != nil {
		os.RemoveAll(mountPath)
		return nil, fmt.Errorf("failed to prepare snapshot: %w", err)
	}

	// Mount the snapshot
	if err := mount.All(mounts, mountPath); err != nil {
		r.snapshotter.Remove(ctx, snapshotKey)
		os.RemoveAll(mountPath)
		return nil, fmt.Errorf("failed to mount snapshot: %w", err)
	}

	// Find kernel and initrd in the mounted filesystem
	kernelPath, initrdPath, err := r.findKernelFiles(mountPath)
	if err != nil {
		mount.UnmountAll(mountPath, 0)
		r.snapshotter.Remove(ctx, snapshotKey)
		os.RemoveAll(mountPath)
		return nil, err
	}

	return &KernelBundle{
		KernelPath:  kernelPath,
		InitrdPath:  initrdPath,
		SnapshotKey: snapshotKey,
		MountPath:   mountPath,
		imageRef:    imageRef,
	}, nil
}

// findKernelFiles searches for kernel and initrd in the mounted image.
func (r *KernelResolver) findKernelFiles(mountPath string) (kernelPath, initrdPath string, err error) {
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

// Release unmounts and removes the snapshot.
func (b *KernelBundle) Release(ctx context.Context, snapshotter snapshots.Snapshotter) error {
	// Unmount
	if err := mount.UnmountAll(b.MountPath, 0); err != nil {
		// Log error but continue cleanup
		fmt.Printf("Warning: failed to unmount %s: %v\n", b.MountPath, err)
	}

	// Remove mount directory
	if err := os.RemoveAll(b.MountPath); err != nil {
		fmt.Printf("Warning: failed to remove mount directory %s: %v\n", b.MountPath, err)
	}

	// Remove snapshot
	if err := snapshotter.Remove(ctx, b.SnapshotKey); err != nil {
		return fmt.Errorf("failed to remove snapshot %s: %w", b.SnapshotKey, err)
	}

	return nil
}

// sanitizeRef sanitizes image reference for use in snapshot key.
func sanitizeRef(ref string) string {
	// Simple sanitization - remove special characters
	result := ""
	for _, c := range ref {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			result += string(c)
		} else {
			result += "-"
		}
	}
	if len(result) > 50 {
		result = result[:50]
	}
	return result
}
