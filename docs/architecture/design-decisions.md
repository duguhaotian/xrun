# Design Decisions

## 1. Daemon-Client Architecture

**Decision**: Split into xrund (daemon) and xrun (client)

**Rationale**:
- Centralized state management
- Multiple clients can connect
- Better resource lifecycle management
- Systemd integration support

**Trade-offs**:
- Additional complexity
- Single point of failure
- Requires privilege escalation

## 2. Containerd Integration

**Decision**: Use containerd for image and snapshot management

**Rationale**:
- Industry standard for container images
- Efficient snapshot mechanisms
- Existing OCI support
- Battle-tested codebase

**Trade-offs**:
- External dependency
- Additional resource overhead

## 3. Image Sharing (View Snapshots)

**Decision**: Use View snapshots for kernel/initrd, shared across VMs

**Rationale**:
- Reduces disk usage
- Faster VM startup
- Simplifies image updates

**Implementation**:
```
OCI Image
    ↓
containerd View (read-only, shared)
    ↓
/var/lib/xrun/images/<digest>/
    ├── boot/vmlinuz  → VM1, VM2, VM3...
    └── boot/initrd
```

## 4. Memory File Management

**Decision**: Use file-based memory backend with RW snapshots

**Rationale**:
- Enables live migration (future)
- Supports snapshot/restore
- Better performance than anonymous memory

**Implementation**:
```
Empty RW Snapshot
    ↓
Create sparse file (memory.file)
    ↓
Mount to VM as memory backend
    ↓
On snapshot: Commit snapshot
```

## 5. Snapshot Labels

**Decision**: Use containerd labels to mark memory snapshots

**Labels**:
- `xrun.microvm/memory-snapshot=true`
- `xrun.microvm/vm-id=<vm-id>`
- `xrun.microvm/snapshot-id=<snapshot-id>`
- `xrun.microvm/created-at=<timestamp>`

**Benefits**:
- Easy discovery on daemon restart
- Metadata persistence
- Garbage collection support

## 6. Network Design

**Decision**: Make networking optional, disabled by default

**Rationale**:
- Not all use cases need networking
- Simplifies initial setup
- Reduces attack surface

**When Enabled**:
- CNI plugin for veth pair creation
- NetNS pool for reuse
- Dynamic expansion on exhaustion

## 7. Run = Create + Start

**Decision**: Merge create and start into single Run operation

**Rationale**:
- Simpler user experience
- Atomic operation
- Reduces intermediate state handling

## 8. Restore Creates New VM

**Decision**: Restore always creates a new VM with new ID

**Rationale**:
- Avoids ID conflicts
- Supports cloning
- Clear separation of original and restored VM

**Implementation**:
- New RW layer from snapshot
- Modified config (shared=off)
- Fresh metadata

## 9. Error Handling Strategy

**Decision**: Cleanup on failure, never leave partial state

**Pattern**:
```go
resource1, err := create()
if err != nil { return err }
defer cleanup1()

resource2, err := create()
if err != nil { return err }
defer cleanup2()

// All resources created, commit
```

## 10. Configuration Hierarchy

**Priority** (high to low):
1. Command-line flags
2. Environment variables
3. Default values

**Rationale**:
- Flexibility for different deployments
- Easy automation
- Sensible defaults
