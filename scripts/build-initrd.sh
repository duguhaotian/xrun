#!/bin/bash
# Build script for Cloud-Hypervisor initrd
# Creates a minimal initrd with busybox and sshd
# Uses dynamic path lookup for better portability

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_DIR="${SCRIPT_DIR}/build"
OUTPUT_DIR="${SCRIPT_DIR}/output"
INITRD_FILE="${OUTPUT_DIR}/initrd.img"

# Function to find binary in PATH
find_binary() {
    local name="$1"
    local path=$(which "$name" 2>/dev/null)
    if [ -z "$path" ]; then
        echo "ERROR: $name not found in PATH" >&2
        exit 1
    fi
    if [ ! -f "$path" ]; then
        echo "ERROR: $name found at $path but file does not exist" >&2
        exit 1
    fi
    echo "$path"
}

# Function to find library file
find_lib() {
    local libname="$1"
    local path=""
    
    # Try ldconfig first
    if command -v ldconfig >/dev/null 2>&1; then
        path=$(ldconfig -p 2>/dev/null | grep "^[[:space:]]*${libname}" | head -1 | awk '{print $NF}')
    fi
    
    # If not found, try common paths
    if [ -z "$path" ]; then
        for dir in /lib /lib64 /usr/lib /usr/lib64 \
                   /lib/x86_64-linux-gnu /usr/lib/x86_64-linux-gnu \
                   /lib/aarch64-linux-gnu /usr/lib/aarch64-linux-gnu; do
            if [ -f "$dir/$libname" ]; then
                path="$dir/$libname"
                break
            fi
        done
    fi
    
    echo "$path"
}

# Function to copy binary and its dependencies
copy_binary_with_deps() {
    local binary_path="$1"
    local dest_dir="$2"
    local dest_name="${3:-$(basename "$binary_path")}"
    
    echo "  Copying: $binary_path"
    cp "$binary_path" "${dest_dir}/${dest_name}"
    chmod 755 "${dest_dir}/${dest_name}"
    
    # Copy dependencies
    local deps=$(ldd "$binary_path" 2>/dev/null | grep -o '/[^ ]*' | sort -u)
    for lib in $deps; do
        if [ -f "$lib" ]; then
            local libname=$(basename "$lib")
            if [ ! -f "${BUILD_DIR}/lib/${libname}" ]; then
                cp -L "$lib" "${BUILD_DIR}/lib/"
            fi
        fi
    done
}

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)
        LD_LINUX="ld-linux-x86-64.so.2"
        ;;
    aarch64)
        LD_LINUX="ld-linux-aarch64.so.1"
        ;;
    *)
        echo "WARNING: Unknown architecture $ARCH, assuming x86_64"
        LD_LINUX="ld-linux-x86-64.so.2"
        ;;
esac

echo "=== Building initrd for Cloud-Hypervisor ==="
echo "Architecture: $ARCH"
echo "Build directory: ${BUILD_DIR}"
echo "Output: ${INITRD_FILE}"

# Find required binaries
echo ""
echo "Locating required binaries..."
BUSYBOX_PATH=$(find_binary busybox)
SSHD_PATH=$(find_binary sshd)
SSH_KEYGEN_PATH=$(find_binary ssh-keygen)

echo "  busybox: $BUSYBOX_PATH"
echo "  sshd: $SSHD_PATH"
echo "  ssh-keygen: $SSH_KEYGEN_PATH"

# Find dynamic linker
echo ""
echo "Locating dynamic linker..."
LD_PATH=$(find_lib "$LD_LINUX")
if [ -z "$LD_PATH" ]; then
    echo "ERROR: Dynamic linker $LD_LINUX not found" >&2
    exit 1
fi
echo "  $LD_LINUX: $LD_PATH"

# Clean up previous build
rm -rf "${BUILD_DIR}" "${OUTPUT_DIR}"
mkdir -p "${BUILD_DIR}" "${OUTPUT_DIR}"

# Create directory structure
echo ""
echo "Creating directory structure..."
mkdir -p "${BUILD_DIR}"/{bin,sbin,etc,lib,lib64,usr,proc,sys,dev,tmp,run,var,root}
mkdir -p "${BUILD_DIR}/etc/ssh"
mkdir -p "${BUILD_DIR}/var/run"
mkdir -p "${BUILD_DIR}/var/empty"

# Copy busybox
echo ""
echo "Copying busybox..."
cp "$BUSYBOX_PATH" "${BUILD_DIR}/bin/busybox"
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
echo ""
echo "Copying sshd and dependencies..."
copy_binary_with_deps "$SSHD_PATH" "${BUILD_DIR}/sbin"

# Copy additional required libraries
echo "Copying additional libraries..."
for libname in libnss_files.so.2 libnss_dns.so.2 libresolv.so.2; do
    libpath=$(find_lib "$libname")
    if [ -n "$libpath" ] && [ -f "$libpath" ]; then
        cp -L "$libpath" "${BUILD_DIR}/lib/"
        echo "  $libname"
    else
        echo "  WARNING: $libname not found"
    fi
done

# Copy dynamic linker
echo "Copying dynamic linker..."
cp "$LD_PATH" "${BUILD_DIR}/lib64/"

# Create /etc/passwd
echo ""
echo "Creating system files..."
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
Subsystem sftp internal-sftp
EOF

# Generate SSH host keys
echo ""
echo "Generating SSH host keys..."
"$SSH_KEYGEN_PATH" -t rsa -f "${BUILD_DIR}/etc/ssh/ssh_host_rsa_key" -N "" -q
"$SSH_KEYGEN_PATH" -t ecdsa -f "${BUILD_DIR}/etc/ssh/ssh_host_ecdsa_key" -N "" -q
"$SSH_KEYGEN_PATH" -t ed25519 -f "${BUILD_DIR}/etc/ssh/ssh_host_ed25519_key" -N "" -q
chmod 600 "${BUILD_DIR}/etc/ssh/"*_key

# Create init script
echo "Creating init script..."
cat > "${BUILD_DIR}/sbin/init" << 'INITEOF'
#!/bin/sh
# Init script for Cloud-Hypervisor microVM

# Mount essential filesystems
echo "Mounting filesystems..."
mount -t proc none /proc
mount -t sysfs none /sys
mount -t devtmpfs none /dev 2>/dev/null || mount -t tmpfs none /dev
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

# Configure network
echo "Configuring network..."
if command -v ip >/dev/null 2>&1; then
    ip link set lo up 2>/dev/null || true
fi

# Create /var/run for sshd
mkdir -p /var/run

# Start sshd
echo "Starting sshd..."
if [ -x /sbin/sshd ]; then
    /sbin/sshd
    echo "  sshd started successfully"
else
    echo "  ERROR: sshd not found or not executable"
fi

# Log startup completion
echo ""
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
echo ""
echo "Creating device nodes..."
mknod -m 666 "${BUILD_DIR}/dev/null" c 1 3 2>/dev/null || true
mknod -m 666 "${BUILD_DIR}/dev/zero" c 1 5 2>/dev/null || true
mknod -m 666 "${BUILD_DIR}/dev/random" c 1 8 2>/dev/null || true
mknod -m 666 "${BUILD_DIR}/dev/urandom" c 1 9 2>/dev/null || true
mknod -m 666 "${BUILD_DIR}/dev/tty" c 5 0 2>/dev/null || true
mknod -m 600 "${BUILD_DIR}/dev/console" c 5 1 2>/dev/null || true

# Create cpio archive
echo ""
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
echo "Architecture: $ARCH"

if [ $SIZE -gt 20971520 ]; then
    echo "WARNING: Initrd exceeds 20MB limit!"
    exit 1
else
    echo "OK: Initrd is within 20MB limit"
fi

echo ""
echo "Output: ${INITRD_FILE}"
ls -lh "${INITRD_FILE}"
