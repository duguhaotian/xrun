# XRun - MicroVM Sandbox Manager

XRun is a daemon-based microVM sandbox management system that provides lifecycle management for microVMs using Cloud-Hypervisor as the VMM backend.

## Features

- **Daemon-Client Architecture**: xrund daemon with gRPC API, xrun CLI client
- **VM Lifecycle Management**: Run, Stop, Delete, Snapshot, Restore operations
- **OCI Image Support**: Kernel and initrd from OCI images with multi-VM sharing
- **Containerd Integration**: Uses containerd snapshots for memory files and image management
- **Optional Networking**: CNI-based networking with netns pool support (disabled by default)
- **Snapshot/Restore**: Full VM state snapshots with memory file preservation

## Architecture

```
┌─────────────────┐     ┌──────────────────┐     ┌─────────────────┐
│   xrun CLI      │────▶│  xrund daemon    │────▶│  containerd     │
│   (client)      │     │  (gRPC API)      │     │  (snapshots)    │
└─────────────────┘     └──────────────────┘     └─────────────────┘
                               │
                               ▼
                        ┌──────────────────┐
                        │  Cloud-Hypervisor │
                        │     VMM Driver    │
                        └──────────────────┘
```

## Quick Start

### Prerequisites

- Linux kernel with KVM support
- containerd running
- Cloud-Hypervisor binary installed
- CNI plugins (optional, for networking)

### Installation

```bash
# Build binaries
go build -o bin/xrund ./cmd/xrund
go build -o bin/xrun ./cmd/xrun

# Install
sudo cp bin/xrund bin/xrun /usr/local/bin/
```

### Running

```bash
# Start daemon
sudo xrund --data-dir=/var/lib/xrun --socket=/run/xrun/xrund.sock

# In another terminal, create a VM
sudo xrun run myvm \
  --image docker.io/myrepo/vm-image:v1.0 \
  --vcpus 2 \
  --memory 1024 \
  --rootfs /path/to/rootfs.img

# List VMs
xrun list

# Stop VM
xrun stop myvm

# Delete VM
xrun delete myvm
```

## Commands

### xrun CLI

| Command | Description |
|---------|-------------|
| `run` | Create and start a new sandbox |
| `stop` | Stop a running sandbox |
| `delete` | Delete a sandbox |
| `list` | List all sandboxes |
| `get` | Get sandbox details |
| `snapshot` | Create a snapshot |
| `restore` | Restore from a snapshot |

## Configuration

### xrund Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--socket` | `/run/xrun/xrund.sock` | Unix socket path |
| `--data-dir` | `/var/lib/xrun` | Data directory |
| `--containerd-address` | `/run/containerd/containerd.sock` | Containerd socket |
| `--snapshot-namespace` | `default` | Containerd snapshot namespace |
| `--snapshotter` | `overlayfs` | Snapshotter name |
| `--ch-binary` | `cloud-hypervisor` | CH binary path |

### Environment Variables

- `XRUN_SOCKET`: Override socket path
- `XRUN_DATA_DIR`: Override data directory
- `XRUN_LOG_LEVEL`: Set log level (debug, info, warn, error)
- `XRUN_CONTAINERD_ADDRESS`: Containerd socket path
- `XRUN_SNAPSHOT_NAMESPACE`: Snapshot namespace

## Documentation

- [Architecture Overview](docs/architecture/overview.md)
- [API Reference](docs/api/grpc-api.md)
- [Usage Guide](docs/usage/quickstart.md)
- [Design Decisions](docs/architecture/design-decisions.md)

## License

MIT License
