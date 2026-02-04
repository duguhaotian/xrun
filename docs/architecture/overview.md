# Architecture Overview

## System Components

### 1. xrund (Daemon)

The daemon is the core component that manages microVM sandboxes. It provides:

- **gRPC API**: Unix socket-based API for client communication
- **VM Lifecycle Management**: Create, start, stop, delete, snapshot, restore
- **Resource Management**: Memory files, network namespaces, OCI images
- **State Persistence**: Metadata storage for sandboxes

### 2. xrun (Client)

Command-line interface for interacting with the daemon:

- User-friendly commands
- Tabular output for listings
- Error handling and validation

### 3. Storage Layer

#### Memory File Management
- Uses containerd snapshots with specific labels
- Each VM has its own RW layer for memory files
- Supports snapshot/commit for VM state preservation

#### Image Cache
- OCI images mounted as View snapshots (read-only)
- Multi-VM sharing of kernel/initrd
- Reference counting for cleanup

### 4. Network Layer (Optional)

- Disabled by default
- CNI plugin integration
- Network namespace pool with dynamic expansion
- IPAM for VM IP allocation

### 5. VMM Driver

Cloud-Hypervisor driver implementation:
- VM creation and configuration
- API communication via Unix socket
- Process lifecycle management

## Data Flow

### Run Flow

```
1. Client sends Run request
2. xrund mounts OCI image (View, shared)
3. xrund creates memory file (RW snapshot)
4. xrund sets up network (if enabled)
5. xrund creates VM via CH driver
6. xrund starts VM process
7. xrund saves metadata
8. Returns VM ID and info
```

### Snapshot Flow

```
1. Client sends Snapshot request
2. xrund pauses VM
3. xrund calls CH API to create VM state
4. xrund commits memory snapshot
5. xrund saves snapshot metadata
6. xrund resumes VM
7. Returns snapshot ID
```

### Restore Flow

```
1. Client sends Restore request
2. xrund creates new RW layer from snapshot
3. xrund configures VM (shared=off)
4. xrund creates new VM
5. xrund restores VM state via CH API
6. xrund saves new metadata
7. Returns new VM ID
```

## Directory Structure

```
/var/lib/xrun/
├── sandboxes/          # VM metadata
├── images/            # OCI image mounts (View)
├── snapshots/         # Memory file RW layers
└── logs/              # Log files
```

## Security Considerations

- Unix socket permissions (root only)
- Network namespace isolation
- No privileged containers required
- Resource limits via cgroup (future)
