# Quick Start Guide

## Installation

### From Source

```bash
# Clone repository
git clone https://github.com/microvm/sandbox.git
cd sandbox

# Build
go build -o bin/xrund ./cmd/xrund
go build -o bin/xrun ./cmd/xrun

# Install
sudo cp bin/xrund bin/xrun /usr/local/bin/
```

### Prerequisites Check

```bash
# Check KVM support
ls /dev/kvm

# Check containerd
sudo ctr version

# Check Cloud-Hypervisor
cloud-hypervisor --version
```

## Basic Usage

### 1. Start the Daemon

```bash
# Basic startup
sudo xrund

# With custom configuration
sudo xrund \
  --data-dir=/var/lib/xrun \
  --socket=/run/xrun/xrund.sock \
  --containerd-address=/run/containerd/containerd.sock \
  --log-level=debug
```

### 2. Create Your First VM

```bash
# Simple VM
sudo xrun run myvm --image docker.io/myrepo/vm-image:v1.0

# With resources
sudo xrun run myvm \
  --image docker.io/myrepo/vm-image:v1.0 \
  --vcpus 2 \
  --memory 1024 \
  --rootfs /path/to/rootfs.img

# With custom kernel command line
sudo xrun run myvm \
  --image docker.io/myrepo/vm-image:v1.0 \
  --cmdline "console=hvc0 root=/dev/vda1 rw quiet"
```

### 3. Manage VMs

```bash
# List all VMs
xrun list

# Get VM details
xrun get myvm

# Stop VM
xrun stop myvm

# Force stop
xrun stop myvm --force

# Delete VM
xrun delete myvm
```

### 4. Snapshots

```bash
# Create snapshot
xrun snapshot myvm --name backup-2024-01-15

# List snapshots (via containerd)
sudo ctr -n default snapshot ls | grep xrun

# Restore to new VM
xrun restore backup-2024-01-15 --new-id myvm-restored

# Verify restored VM
xrun get myvm-restored
```

## Advanced Usage

### Networking Setup

Enable networking (disabled by default):

```bash
# Start daemon with networking
sudo xrund \
  --network-enabled \
  --cni-plugin=/opt/cni/bin/bridge \
  --cni-config=/etc/cni/net.d/10-bridge.conf

# Create VM with network
sudo xrun run myvm --image docker.io/myrepo/vm-image:v1.0
# VM will have network access via CNI
```

### Using NetNS Pool

```bash
# Enable pool for better performance
sudo xrund \
  --network-enabled \
  --use-netns-pool \
  --netns-pool-size=20
```

### Custom Containerd Namespace

```bash
# Use custom namespace for isolation
sudo xrund --snapshot-namespace=xrun-vms
```

## Troubleshooting

### Daemon Won't Start

```bash
# Check socket permissions
ls -la /run/xrun/

# Check logs
cat /var/lib/xrun/logs/xrund.log

# Try debug mode
sudo xrund --log-level=debug
```

### VM Fails to Start

```bash
# Check CH logs
cat /var/lib/xrun/vms/<vm-id>/vm.log
cat /var/lib/xrun/vms/<vm-id>/vm.stderr.log

# Verify image mount
ls /var/lib/xrun/images/

# Check memory file
ls /var/lib/xrun/snapshots/
```

### Network Issues

```bash
# Check CNI configuration
cat /etc/cni/net.d/10-bridge.conf

# Verify netns
sudo ip netns list

# Check veth pairs
ip link show
```

## Best Practices

1. **Use Labels**: Tag VMs for easier management
   ```bash
   xrun run myvm --image ... --label env=prod --label app=web
   ```

2. **Regular Snapshots**: Create snapshots before updates
   ```bash
   xrun snapshot myvm --name pre-update-$(date +%Y%m%d)
   ```

3. **Resource Planning**: Monitor resource usage
   ```bash
   xrun list  # Shows vcpus and memory
   ```

4. **Clean Up**: Delete unused VMs and snapshots
   ```bash
   xrun delete old-vm
   sudo ctr -n default snapshot rm <old-snapshot>
   ```

## Example Workflows

### CI/CD Pipeline

```bash
# Start test VM
xrun run test-vm --image ci-image:latest --vcpus 4 --memory 4096

# Run tests
# ... test commands ...

# Create snapshot if tests pass
xrun snapshot test-vm --name tests-passed

# Cleanup
xrun delete test-vm
```

### Development Environment

```bash
# Create dev VM
xrun run dev-vm --image dev-image:latest

# Work...

# Snapshot before major changes
xrun snapshot dev-vm --name baseline

# If something breaks, restore
xrun restore baseline --new-id dev-vm-fixed
xrun delete dev-vm
```
