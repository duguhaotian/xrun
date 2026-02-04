# gRPC API Reference

## Service: SandboxService

### Run

Create and start a new sandbox.

**Request**: `RunRequest`
```protobuf
message RunRequest {
  string id = 1;              // Sandbox ID (required)
  string image = 2;           // OCI image reference (required)
  string rootfs = 3;          // Rootfs disk path (optional)
  uint32 vcpus = 4;           // Number of vCPUs (default: 1)
  uint32 memory_mb = 5;       // Memory in MB (default: 512)
  string cmdline = 6;         // Kernel command line
  map<string, string> labels = 7;  // Additional labels
}
```

**Response**: `RunResponse`
```protobuf
message RunResponse {
  string id = 1;              // Sandbox ID
  string state = 2;           // Current state (running)
  int32 pid = 3;              // Process ID
  string ip_address = 4;      // veth host IP (if networking enabled)
}
```

**Example**:
```bash
xrun run myvm --image docker.io/myrepo/vm-image:v1.0 --vcpus 2 --memory 1024
```

### Stop

Stop a running sandbox.

**Request**: `StopRequest`
```protobuf
message StopRequest {
  string id = 1;              // Sandbox ID
  bool force = 2;             // Force stop (default: false)
}
```

**Response**: `StopResponse`
```protobuf
message StopResponse {
  string id = 1;
  string state = 2;           // stopped
}
```

**Example**:
```bash
xrun stop myvm
xrun stop myvm --force
```

### Delete

Delete a sandbox.

**Request**: `DeleteRequest`
```protobuf
message DeleteRequest {
  string id = 1;              // Sandbox ID
  bool force = 2;             // Force delete if running
}
```

**Response**: `DeleteResponse`
```protobuf
message DeleteResponse {
  string id = 1;
}
```

**Example**:
```bash
xrun delete myvm
xrun delete myvm --force
```

### Get

Get sandbox details.

**Request**: `GetRequest`
```protobuf
message GetRequest {
  string id = 1;              // Sandbox ID
}
```

**Response**: `GetResponse`
```protobuf
message GetResponse {
  Sandbox sandbox = 1;
}
```

**Sandbox Fields**:
```protobuf
message Sandbox {
  string id = 1;
  string state = 2;           // pending, running, paused, stopped
  int32 pid = 3;
  uint32 vcpus = 4;
  uint32 memory_mb = 5;
  string image = 6;
  string rootfs = 7;
  string ip_address = 8;
  string created_at = 9;
  map<string, string> labels = 10;
}
```

**Example**:
```bash
xrun get myvm
```

### List

List all sandboxes.

**Request**: `ListRequest`
```protobuf
message ListRequest {
  map<string, string> filters = 1;  // Optional filters
}
```

**Response**: `ListResponse`
```protobuf
message ListResponse {
  repeated Sandbox sandboxes = 1;
}
```

**Example**:
```bash
xrun list
```

### Snapshot

Create a snapshot of a sandbox.

**Request**: `SnapshotRequest`
```protobuf
message SnapshotRequest {
  string id = 1;              // Sandbox ID
  string name = 2;            // Snapshot name
  map<string, string> labels = 3;
}
```

**Response**: `SnapshotResponse`
```protobuf
message SnapshotResponse {
  string snapshot_id = 1;     // Unique snapshot ID
  string created_at = 2;
}
```

**Example**:
```bash
xrun snapshot myvm --name snap1
```

**Notes**:
- VM is paused during snapshot
- Includes memory state and VM state
- Snapshot is committed to containerd

### Restore

Restore a sandbox from a snapshot.

**Request**: `RestoreRequest`
```protobuf
message RestoreRequest {
  string snapshot_id = 1;     // Snapshot ID to restore from
  string new_id = 2;          // New sandbox ID (creates new VM)
}
```

**Response**: `RestoreResponse`
```protobuf
message RestoreResponse {
  string id = 1;              // New sandbox ID
  string state = 2;           // running
}
```

**Example**:
```bash
xrun restore snap1 --new-id myvm2
```

**Notes**:
- Creates a new VM with new ID
- Original VM remains unchanged
- Memory config uses shared=off

## Error Codes

| Error | Description |
|-------|-------------|
| `AlreadyExists` | Sandbox ID already exists |
| `NotFound` | Sandbox or snapshot not found |
| `InvalidArgument` | Invalid request parameters |
| `FailedPrecondition` | VM in wrong state for operation |
| `Internal` | Internal server error |

## Socket Path

Default: `/run/xrun/xrund.sock`

Override with `--socket` flag or `XRUN_SOCKET` environment variable.
