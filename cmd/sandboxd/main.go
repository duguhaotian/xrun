package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/microvm/sandbox/pkg/sandbox"
	"github.com/microvm/sandbox/pkg/vmm"
	"github.com/microvm/sandbox/pkg/vmm/cloudhypervisor"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	// Setup VMM factory
	factory := vmm.NewFactory()

	// Register Cloud-Hypervisor driver (default)
	chDriver := cloudhypervisor.NewDriver("cloud-hypervisor", "/var/lib/sandbox/vms")
	factory.Register("cloud-hypervisor", chDriver)

	// Setup sandbox manager
	manager, err := sandbox.NewManager(sandbox.Config{
		DataDir:    "/var/lib/sandbox",
		DefaultVMM: "cloud-hypervisor",
	}, factory)
	if err != nil {
		return fmt.Errorf("failed to create sandbox manager: %w", err)
	}
	defer manager.Close()

	// Parse command
	if len(os.Args) < 2 {
		return printUsage()
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "create":
		return handleCreate(ctx, manager, args)
	case "start":
		return handleStart(ctx, manager, args)
	case "stop":
		return handleStop(ctx, manager, args)
	case "list":
		return handleList(ctx, manager, args)
	case "snapshot":
		return handleSnapshot(ctx, manager, args)
	case "restore":
		return handleRestore(ctx, manager, args)
	case "pause":
		return handlePause(ctx, manager, args)
	case "resume":
		return handleResume(ctx, manager, args)
	case "delete":
		return handleDelete(ctx, manager, args)
	case "help", "--help", "-h":
		return printUsage()
	default:
		return fmt.Errorf("unknown command: %s", cmd)
	}
}

func printUsage() error {
	fmt.Println(`MicroVM Sandbox Manager

Usage:
  sandboxd <command> [options]

Commands:
  create     Create a new sandbox
  start      Start an existing sandbox
  stop       Stop a running sandbox
  list       List all sandboxes
  snapshot   Create a snapshot of a sandbox
  restore    Restore a sandbox from snapshot
  pause      Pause a running sandbox
  resume     Resume a paused sandbox
  delete     Delete a sandbox
  help       Show this help message

Create Options:
  --id         Sandbox identifier (required)
  --kernel     Path to kernel image (required)
  --initrd     Path to initrd/initramfs image (required)
  --rootfs     Path to root filesystem disk image (required, passed as virtio disk)
  --vcpus      Number of vCPUs (default: 1)
  --memory     Memory size in MB (default: 512)
  --mem-backend Memory backend type: anonymous, file (default: anonymous)
  --mem-file   Path to memory backend file (when mem-backend=file)
  --cmdline    Kernel command line (default: "console=hvc0 root=/dev/vda1 rw")
  --vmm        VMM driver to use (default: cloud-hypervisor)
  --start      Auto-start the VM after creation

Examples:
  # Create with kernel+initrd, rootfs as virtio disk
  sandboxd create --id myvm --kernel /path/to/vmlinux --initrd /path/to/initrd.img --rootfs /path/to/rootfs.img --vcpus 2 --memory 1024

  # With custom cmdline
  sandboxd create --id myvm --kernel /path/to/vmlinux --initrd /path/to/initrd.img --rootfs /path/to/rootfs.img --cmdline "console=ttyS0 root=/dev/vda1 rw quiet"

  sandboxd start --id myvm
  sandboxd stop --id myvm
  sandboxd list
  sandboxd snapshot --id myvm --dest /path/to/snapshot
  sandboxd restore --id myvm --source /path/to/snapshot`)
	return nil
}

func handleCreate(ctx context.Context, manager *sandbox.Manager, args []string) error {
	var opts sandbox.CreateOptions
	var id string
	var memSizeMB uint32
	var memBackend string
	var memBackendPath string
	var kernelPath string
	var initrdPath string
	var rootfsPath string
	var cmdline string

	// Simple flag parsing
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--id":
			i++
			if i < len(args) {
				id = args[i]
			}
		case "--kernel":
			i++
			if i < len(args) {
				kernelPath = args[i]
			}
		case "--initrd":
			i++
			if i < len(args) {
				initrdPath = args[i]
			}
		case "--rootfs":
			i++
			if i < len(args) {
				rootfsPath = args[i]
			}
		case "--cmdline":
			i++
			if i < len(args) {
				cmdline = args[i]
			}
		case "--vcpus":
			i++
			if i < len(args) {
				// Parse uint32
				fmt.Sscanf(args[i], "%d", &opts.VCPUs)
			}
		case "--memory":
			i++
			if i < len(args) {
				// Parse uint32
				fmt.Sscanf(args[i], "%d", &memSizeMB)
			}
		case "--mem-backend":
			i++
			if i < len(args) {
				memBackend = args[i]
			}
		case "--mem-file":
			i++
			if i < len(args) {
				memBackendPath = args[i]
			}
		case "--vmm":
			i++
			if i < len(args) {
				opts.VMM = args[i]
			}
		case "--start":
			opts.AutoStart = true
		}
	}

	if id == "" {
		return fmt.Errorf("--id is required")
	}
	if kernelPath == "" {
		return fmt.Errorf("--kernel is required")
	}
	if initrdPath == "" {
		return fmt.Errorf("--initrd is required")
	}
	if rootfsPath == "" {
		return fmt.Errorf("--rootfs is required")
	}

	// Default values
	if opts.VCPUs == 0 {
		opts.VCPUs = 1
	}
	if memSizeMB == 0 {
		memSizeMB = 512
	}
	if cmdline == "" {
		cmdline = "console=hvc0 root=/dev/vda1 rw"
	}

	// Build boot configuration (kernel + initrd)
	opts.Boot = vmm.BootConfig{
		KernelPath: kernelPath,
		InitrdPath: initrdPath,
		Cmdline:    cmdline,
	}

	// Build rootfs as virtio disk
	opts.RootFS = vmm.DiskConfig{
		Path:     rootfsPath,
		ReadOnly: false,
		Format:   "raw",
	}

	// Build memory configuration
	opts.Memory.SizeMB = memSizeMB
	opts.Memory.Backend = vmm.MemoryBackendAnonymous // default
	if memBackend == "file" {
		opts.Memory.Backend = vmm.MemoryBackendFile
		opts.Memory.BackendPath = memBackendPath
	}

	vm, err := manager.Create(ctx, id, opts)
	if err != nil {
		return err
	}

	info, _ := vm.Info(ctx)
	fmt.Printf("Created sandbox %s (State: %s)\n", id, info.State)
	return nil
}

func handleStart(ctx context.Context, manager *sandbox.Manager, args []string) error {
	var id string
	for i := 0; i < len(args); i++ {
		if args[i] == "--id" && i+1 < len(args) {
			id = args[i+1]
			i++
		}
	}
	if id == "" {
		return fmt.Errorf("--id is required")
	}

	vm, err := manager.Get(id)
	if err != nil {
		return err
	}

	if err := vm.Start(ctx); err != nil {
		return err
	}

	fmt.Printf("Started sandbox %s\n", id)
	return nil
}

func handleStop(ctx context.Context, manager *sandbox.Manager, args []string) error {
	var id string
	var force bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--id":
			i++
			if i < len(args) {
				id = args[i]
			}
		case "--force":
			force = true
		}
	}
	if id == "" {
		return fmt.Errorf("--id is required")
	}

	if err := manager.Stop(ctx, id, force); err != nil {
		return err
	}

	fmt.Printf("Stopped sandbox %s\n", id)
	return nil
}

func handleList(ctx context.Context, manager *sandbox.Manager, args []string) error {
	vms, err := manager.List(ctx)
	if err != nil {
		return err
	}

	if len(vms) == 0 {
		fmt.Println("No sandboxes found")
		return nil
	}

	fmt.Printf("%-20s %-10s %-6s %-8s %-10s\n", "ID", "STATE", "PID", "VCPUS", "MEMORY")
	fmt.Println(string(make([]byte, 60)))
	for _, vm := range vms {
		fmt.Printf("%-20s %-10s %-6d %-8d %-10d\n",
			vm.ID, vm.State, vm.PID, vm.VCPUs, vm.MemoryMB)
	}
	return nil
}

func handleSnapshot(ctx context.Context, manager *sandbox.Manager, args []string) error {
	var id, dest string
	var compress bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--id":
			i++
			if i < len(args) {
				id = args[i]
			}
		case "--dest":
			i++
			if i < len(args) {
				dest = args[i]
			}
		case "--compress":
			compress = true
		}
	}
	if id == "" || dest == "" {
		return fmt.Errorf("--id and --dest are required")
	}

	opts := sandbox.SnapshotOptions{
		Destination: dest,
		Compress:    compress,
	}

	if err := manager.Snapshot(ctx, id, opts); err != nil {
		return err
	}

	fmt.Printf("Created snapshot of sandbox %s at %s\n", id, dest)
	return nil
}

func handleRestore(ctx context.Context, manager *sandbox.Manager, args []string) error {
	var id, source string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--id":
			i++
			if i < len(args) {
				id = args[i]
			}
		case "--source":
			i++
			if i < len(args) {
				source = args[i]
			}
		}
	}
	if id == "" || source == "" {
		return fmt.Errorf("--id and --source are required")
	}

	opts := sandbox.RestoreOptions{
		Source: source,
	}

	if err := manager.Restore(ctx, id, opts); err != nil {
		return err
	}

	fmt.Printf("Restored sandbox %s from %s\n", id, source)
	return nil
}

func handlePause(ctx context.Context, manager *sandbox.Manager, args []string) error {
	var id string
	for i := 0; i < len(args); i++ {
		if args[i] == "--id" && i+1 < len(args) {
			id = args[i+1]
			i++
		}
	}
	if id == "" {
		return fmt.Errorf("--id is required")
	}

	if err := manager.Pause(ctx, id); err != nil {
		return err
	}

	fmt.Printf("Paused sandbox %s\n", id)
	return nil
}

func handleResume(ctx context.Context, manager *sandbox.Manager, args []string) error {
	var id string
	for i := 0; i < len(args); i++ {
		if args[i] == "--id" && i+1 < len(args) {
			id = args[i+1]
			i++
		}
	}
	if id == "" {
		return fmt.Errorf("--id is required")
	}

	if err := manager.Resume(ctx, id); err != nil {
		return err
	}

	fmt.Printf("Resumed sandbox %s\n", id)
	return nil
}

func handleDelete(ctx context.Context, manager *sandbox.Manager, args []string) error {
	var id string
	var force bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--id":
			i++
			if i < len(args) {
				id = args[i]
			}
		case "--force":
			force = true
		}
	}
	if id == "" {
		return fmt.Errorf("--id is required")
	}

	if err := manager.Delete(ctx, id, force); err != nil {
		return err
	}

	fmt.Printf("Deleted sandbox %s\n", id)
	return nil
}
