// Package image provides OCI image management for microVM sandboxes.
package image

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/snapshots"
	"github.com/microvm/sandbox/pkg/log"
)

// Cache manages OCI image mounts for kernel/initrd sharing.
type Cache struct {
	client      *containerd.Client
	snapshotter snapshots.Snapshotter
	namespace   string
	dataDir     string

	// Memory index: image-ref -> mount info
	mounts map[string]*MountInfo
	mu     sync.RWMutex
}

// MountInfo holds information about an image mount.
type MountInfo struct {
	ImageRef    string
	Digest      string // Image digest
	MountPath   string // /var/lib/xrun/images/<digest>/
	SnapshotKey string // view snapshot key
	RefCount    int    // Reference count
	KernelPath  string
	InitrdPath  string
}

// NewCache creates a new image cache.
func NewCache(client *containerd.Client, snapshotter snapshots.Snapshotter, namespace, dataDir string) *Cache {
	return &Cache{
		client:      client,
		snapshotter: snapshotter,
		namespace:   namespace,
		dataDir:     filepath.Join(dataDir, "images"),
		mounts:      make(map[string]*MountInfo),
	}
}

// Init initializes the cache and rebuilds from disk.
func (c *Cache) Init(ctx context.Context) error {
	// Ensure images directory exists
	if err := os.MkdirAll(c.dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create images directory: %w", err)
	}

	// Scan existing mounts
	entries, err := os.ReadDir(c.dataDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		mountPath := filepath.Join(c.dataDir, entry.Name())
		if info := c.checkExistingMount(mountPath); info != nil {
			c.mu.Lock()
			c.mounts[info.ImageRef] = info
			c.mu.Unlock()
			log.Info("Restored image mount: %s -> %s", info.ImageRef, mountPath)
		}
	}

	return nil
}

// GetOrMount gets or creates a mount for an image.
func (c *Cache) GetOrMount(ctx context.Context, imageRef string) (*MountInfo, error) {
	ctx = namespaces.WithNamespace(ctx, c.namespace)

	// Check memory cache first
	c.mu.RLock()
	if info, ok := c.mounts[imageRef]; ok {
		c.mu.RUnlock()
		c.mu.Lock()
		info.RefCount++
		c.mu.Unlock()
		return info, nil
	}
	c.mu.RUnlock()

	// Pull image if needed
	img, err := c.client.GetImage(ctx, imageRef)
	if err != nil {
		// Image not found, pull it
		img, err = c.client.Pull(ctx, imageRef, containerd.WithPullUnpack)
		if err != nil {
			return nil, fmt.Errorf("failed to pull image %s: %w", imageRef, err)
		}
	}

	// Get image rootfs
	rootfs, err := img.RootFS(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get image rootfs: %w", err)
	}

	if len(rootfs) == 0 {
		return nil, fmt.Errorf("image has no rootfs layers")
	}

	// Use the last layer digest
	layerDigest := rootfs[len(rootfs)-1].String()

	// Check memory cache again (concurrent case)
	c.mu.Lock()
	if info, ok := c.mounts[imageRef]; ok {
		info.RefCount++
		c.mu.Unlock()
		return info, nil
	}

	// Check disk cache
	mountPath := filepath.Join(c.dataDir, layerDigest)
	if info := c.checkExistingMount(mountPath); info != nil {
		info.ImageRef = imageRef
		c.mounts[imageRef] = info
		c.mu.Unlock()
		return info, nil
	}

	// Create new mount
	snapshotKey := fmt.Sprintf("xrun-image-%s-view", layerDigest[:16])

	// Create view snapshot (read-only)
	mounts, err := c.snapshotter.View(ctx, snapshotKey, layerDigest)
	if err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("failed to create view snapshot: %w", err)
	}

	// Create mount directory
	if err := os.MkdirAll(mountPath, 0755); err != nil {
		c.snapshotter.Remove(ctx, snapshotKey)
		c.mu.Unlock()
		return nil, fmt.Errorf("failed to create mount directory: %w", err)
	}

	// Mount the snapshot
	if err := mount.All(mounts, mountPath); err != nil {
		os.RemoveAll(mountPath)
		c.snapshotter.Remove(ctx, snapshotKey)
		c.mu.Unlock()
		return nil, fmt.Errorf("failed to mount snapshot: %w", err)
	}

	// Find kernel and initrd
	kernelPath, initrdPath := c.findKernelFiles(mountPath)

	info := &MountInfo{
		ImageRef:    imageRef,
		Digest:      layerDigest,
		MountPath:   mountPath,
		SnapshotKey: snapshotKey,
		RefCount:    1,
		KernelPath:  kernelPath,
		InitrdPath:  initrdPath,
	}

	c.mounts[imageRef] = info
	c.mu.Unlock()

	log.Info("Created image mount: %s -> %s", imageRef, mountPath)
	return info, nil
}

// Release releases a reference to an image mount.
func (c *Cache) Release(imageRef string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if info, ok := c.mounts[imageRef]; ok {
		info.RefCount--
		if info.RefCount <= 0 {
			// Keep mounted for reuse, just mark as unused
			log.Debug("Image %s now unused but keeping mounted", imageRef)
		}
	}
}

// checkExistingMount checks if a mount path already has a valid mount.
func (c *Cache) checkExistingMount(mountPath string) *MountInfo {
	// Check if boot directory exists
	bootDir := filepath.Join(mountPath, "boot")
	if _, err := os.Stat(bootDir); err != nil {
		return nil
	}

	// Find kernel files
	kernelPath, initrdPath := c.findKernelFiles(mountPath)
	if kernelPath == "" {
		return nil
	}

	return &MountInfo{
		MountPath:  mountPath,
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		RefCount:   0,
	}
}

// findKernelFiles searches for kernel and initrd in the mounted image.
func (c *Cache) findKernelFiles(mountPath string) (kernelPath, initrdPath string) {
	// Common kernel file names
	kernelPaths := []string{
		"boot/vmlinuz",
		"boot/bzImage",
		"boot/kernel",
		"vmlinuz",
		"bzImage",
		"kernel",
	}

	// Common initrd file names
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

	// Search for initrd (optional)
	for _, relPath := range initrdPaths {
		fullPath := filepath.Join(mountPath, relPath)
		if info, err := os.Stat(fullPath); err == nil && !info.IsDir() {
			initrdPath = fullPath
			break
		}
	}

	return
}

// Close closes the cache and unmounts all images.
func (c *Cache) Close(ctx context.Context) error {
	ctx = namespaces.WithNamespace(ctx, c.namespace)

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, info := range c.mounts {
		// Unmount
		if err := mount.UnmountAll(info.MountPath, 0); err != nil {
			log.Warn("Failed to unmount %s: %v", info.MountPath, err)
		}

		// Remove mount directory
		if err := os.RemoveAll(info.MountPath); err != nil {
			log.Warn("Failed to remove mount directory %s: %v", info.MountPath, err)
		}

		// Remove snapshot
		if err := c.snapshotter.Remove(ctx, info.SnapshotKey); err != nil {
			log.Warn("Failed to remove snapshot %s: %v", info.SnapshotKey, err)
		}
	}

	return nil
}
