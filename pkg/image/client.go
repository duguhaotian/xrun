// Package image provides OCI image and containerd snapshot integration for microVM sandboxes.
package image

import (
	"context"
	"fmt"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/snapshots"
)

// Client wraps containerd client for image operations.
type Client struct {
	client      *containerd.Client
	namespace   string
	snapshotter string
}

// NewClient creates a new image client.
func NewClient(address, namespace, snapshotter string) (*Client, error) {
	if address == "" {
		address = "/run/containerd/containerd.sock"
	}
	if namespace == "" {
		namespace = "microvm-sandbox"
	}
	if snapshotter == "" {
		snapshotter = "overlayfs"
	}

	client, err := containerd.New(address)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to containerd: %w", err)
	}

	return &Client{
		client:      client,
		namespace:   namespace,
		snapshotter: snapshotter,
	}, nil
}

// Close closes the containerd client.
func (c *Client) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// PullImage pulls an OCI image.
func (c *Client) PullImage(ctx context.Context, ref string) (containerd.Image, error) {
	ctx = namespaces.WithNamespace(ctx, c.namespace)
	return c.client.Pull(ctx, ref, containerd.WithPullUnpack)
}

// GetImage returns an existing image.
func (c *Client) GetImage(ctx context.Context, ref string) (containerd.Image, error) {
	ctx = namespaces.WithNamespace(ctx, c.namespace)
	return c.client.GetImage(ctx, ref)
}

// Snapshotter returns the snapshotter service.
func (c *Client) Snapshotter() snapshots.Snapshotter {
	return c.client.SnapshotService(c.snapshotter)
}

// Namespace returns the containerd namespace.
func (c *Client) Namespace() string {
	return c.namespace
}
