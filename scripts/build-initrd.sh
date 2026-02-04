#!/bin/bash
# Build script for Cloud-Hypervisor initrd
# Creates a minimal initrd with busybox and sshd

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_DIR="${SCRIPT_DIR}/build"
OUTPUT_DIR="${SCRIPT_DIR}/output"
INITRD_FILE="${OUTPUT_DIR}/initrd.img"

# Clean up previous build
rm -rf "${BUILD_DIR}" "${OUTPUT_DIR}"
mkdir -p "${BUILD_DIR}" "${OUTPUT_DIR}"

echo "=== Building initrd for Cloud-Hypervisor ==="
echo "Build directory: ${BUILD_DIR}"
echo "Output: ${INITRD_FILE}"

# Create directory structure
echo "Creating directory structure..."
mkdir -p "${BUILD_DIR}"/{bin,sbin,etc,lib,lib64,usr,proc,sys,dev,tmp,run,var,root}
mkdir -p "${BUILD_DIR}/etc/ssh"
mkdir -p "${BUILD_DIR}/var/run"
mkdir -p "${BUILD_DIR}/var/empty"

# Copy busybox
echo "Copying busybox..."
cp /bin/busybox "${BUILD_DIR}/bin/"
chmod 755 "${BUILD_DIR}/bin/busybox"

# Create busybox symlinks
echo "Creating busybox symlinks..."
cd "${BUILD_DIR}/bin"
for cmd in sh ls cat cp mv rm mkdir rmdir echo printf test [ [[ sleep mount umount; do
    ln -sf busybox "${cmd}"
done
cd "${SCRIPT_DIR}"

# Create sbin symlinks for busybox (excluding init)
cd "${BUILD_DIR}/sbin"
for cmd in halt reboot poweroff; do
    ln -sf ../bin/busybox "${cmd}"
done
cd "${SCRIPT_DIR}"

# Copy sshd and dependencies
echo "Copying sshd and dependencies..."
cp /usr/sbin/sshd "${BUILD_DIR}/sbin/"
chmod 755 "${BUILD_DIR}/sbin/sshd"

# Copy required libraries
echo "Copying required libraries..."
for lib in $(ldd /usr/sbin/sshd | grep -o '/lib[^ ]*' | sort -u); do
    if [ -f "$lib" ]; then
        cp -L "$lib" "${BUILD_DIR}/lib/"
    fi
done

# Copy additional required libraries
for lib in /lib/x86_64-linux-gnu/libnss_files.so.2 \
           /lib/x86_64-linux-gnu/libnss_dns.so.2 \
           /lib/x86_64-linux-gnu/libresolv.so.2; do
    if [ -f "$lib" ]; then
        cp -L "$lib" "${BUILD_DIR}/lib/"
    fi
done

# Copy ld-linux
cp /lib64/ld-linux-x86-64.so.2 "${BUILD_DIR}/lib64/"

# Create /etc/passwd
cat > "${BUILD_DIR}/etc/passwd" << 'EOF'
root:x:0:0:root:/root:/bin/sh
EOF

# Create /etc/group
cat > "${BUILD_DIR}/etc/group" << 'EOF'
root:x:0:
EOF

# Create minimal sshd_config
cat > "${BUILD_DIR}/etc/ssh/sshd_config" << 'EOF'
Port 22
PermitRootLogin yes
PasswordAuthentication yes
PermitEmptyPasswords yes
UsePAM no
HostKey /etc/ssh/ssh_host_rsa_key
HostKey /etc/ssh/ssh_host_ecdsa_key
HostKey /etc/ssh/ssh_host_ed25519_key
Subsystem sftp /usr/lib/openssh/sftp-server
EOF

# Generate SSH host keys
echo "Generating SSH host keys..."
ssh-keygen -t rsa -f "${BUILD_DIR}/etc/ssh/ssh_host_rsa_key" -N "" -q
ssh-keygen -t ecdsa -f "${BUILD_DIR}/etc/ssh/ssh_host_ecdsa_key" -N "" -q
ssh-keygen -t ed25519 -f "${BUILD_DIR}/etc/ssh/ssh_host_ed25519_key" -N "" -q
chmod 600 "${BUILD_DIR}/etc/ssh/"*_key

# Create init script (must be a real file, not symlink)
cat > "${BUILD_DIR}/sbin/init" << 'INITEOF'
#!/bin/sh
# Init script for Cloud-Hypervisor microVM

# Mount essential filesystems
echo "Mounting filesystems..."
mount -t proc none /proc
mount -t sysfs none /sys
mount -t devtmpfs none /dev
mount -t tmpfs none /tmp

# Create necessary device nodes
[ -e /dev/null ] || mknod -m 666 /dev/null c 1 3
[ -e /dev/zero ] || mknod -m 666 /dev/zero c 1 5
[ -e /dev/random ] || mknod -m 666 /dev/random c 1 8
[ -e /dev/urandom ] || mknod -m 666 /dev/urandom c 1 9
[ -e /dev/tty ] || mknod -m 666 /dev/tty c 5 0
[ -e /dev/console ] || mknod -m 600 /dev/console c 5 1

# Set hostname
echo "cloud-hypervisor-vm" > /proc/sys/kernel/hostname

# Configure network (will be configured by cloud-hypervisor)
echo "Configuring network..."
ip link set lo up 2>/dev/null || true

# Create /var/run for sshd
mkdir -p /var/run

# Start sshd
echo "Starting sshd..."
/sbin/sshd

# Log startup completion
echo "=== Init complete, system ready ==="
echo "SSH server listening on port 22"
echo "Root login enabled (no password)"

# Keep init running
while true; do
    sleep 3600
done
INITEOF

chmod 755 "${BUILD_DIR}/sbin/init"

# Create device nodes
echo "Creating device nodes..."
mknod -m 666 "${BUILD_DIR}/dev/null" c 1 3 2>/dev/null || true
mknod -m 666 "${BUILD_DIR}/dev/zero" c 1 5 2>/dev/null || true
mknod -m 666 "${BUILD_DIR}/dev/random" c 1 8 2>/dev/null || true
mknod -m 666 "${BUILD_DIR}/dev/urandom" c 1 9 2>/dev/null || true
mknod -m 666 "${BUILD_DIR}/dev/tty" c 5 0 2>/dev/null || true
mknod -m 600 "${BUILD_DIR}/dev/console" c 5 1 2>/dev/null || true

# Create cpio archive
echo "Creating initrd archive..."
cd "${BUILD_DIR}"
find . | cpio -o -H newc | gzip -9 > "${INITRD_FILE}"
cd "${SCRIPT_DIR}"

# Check size
SIZE=$(stat -c%s "${INITRD_FILE}")
SIZE_MB=$(echo "scale=2; $SIZE / 1024 / 1024" | bc)
echo ""
echo "=== Build complete ==="
echo "Initrd size: ${SIZE_MB} MB (${SIZE} bytes)"

if [ $SIZE -gt 20971520 ]; then
    echo "WARNING: Initrd exceeds 20MB limit!"
    exit 1
else
    echo "OK: Initrd is within 20MB limit"
fi

echo ""
echo "Output: ${INITRD_FILE}"
ls -lh "${INITRD_FILE}"
