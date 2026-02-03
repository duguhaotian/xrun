package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/microvm/sandbox/pkg/log"
	"github.com/microvm/sandbox/pkg/sandbox"
	"github.com/microvm/sandbox/pkg/vmm"
	"github.com/microvm/sandbox/pkg/vmm/cloudhypervisor"
)

// CreateFlags holds the flags for the create command.
type CreateFlags struct {
	ID         string
	Image      string
	RootFS     string
	VCPUs      uint
	Memory     uint
	MemBackend string
	MemFile    string
	Cmdline    string
	VMM        string
	AutoStart  bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Initialize logging
	if err := log.Init("/var/log/xrun"); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to initialize logging: %v\n", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log.Info("Starting xrun...")

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
		DataDir:             "/var/lib/sandbox",
		DefaultVMM:          "cloud-hypervisor",
		ContainerdAddress:   "/run/containerd/containerd.sock",
		ContainerdNamespace: "default",
		Snapshotter:         "overlayfs",
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

	switch cmd {
	case "create":
		return handleCreate(ctx, manager, os.Args[2:])
	case "start":
		return handleStart(ctx, manager, os.Args[2:])
	case "stop":
		return handleStop(ctx, manager, os.Args[2:])
	case "list":
		return handleList(ctx, manager, os.Args[2:])
	case "snapshot":
		return handleSnapshot(ctx, manager, os.Args[2:])
	case "restore":
		return handleRestore(ctx, manager, os.Args[2:])
	case "pause":
		return handlePause(ctx, manager, os.Args[2:])
	case "resume":
		return handleResume(ctx, manager, os.Args[2:])
	case "delete":
		return handleDelete(ctx, manager, os.Args[2:])
	case "help", "--help", "-h":
		return printUsage()
	default:
		return fmt.Errorf("unknown command: %s", cmd)
	}
}

func printUsage() error {
	fmt.Println(`MicroVM Sandbox Manager

Usage:
  xrun <command> [options]

Commands:
  create     Create a new sandbox from OCI image
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
  -id            Sandbox identifier (required)
  -image         OCI image reference containing kernel and initrd (required)
  -rootfs        Path to root filesystem disk image (optional)
  -vcpus         Number of vCPUs (default: 1)
  -memory        Memory size in MB (default: 512)
  -mem-backend   Memory backend type: anonymous, file (default: anonymous)
  -mem-file      Path to memory backend file (when mem-backend=file)
  -cmdline       Kernel command line (default: "console=hvc0 root=/dev/vda1 rw")
  -vmm           VMM driver to use (default: cloud-hypervisor)
  -start         Auto-start the VM after creation

Examples:
  # Create sandbox from OCI image
  xrun create -id myvm -image docker.io/myrepo/vm-image:v1.0 -rootfs /path/to/rootfs.img -vcpus 2 -memory 1024

  # With custom cmdline
  xrun create -id myvm -image docker.io/myrepo/vm-image:v1.0 -cmdline "console=ttyS0 root=/dev/vda1 rw quiet"

  xrun start -id myvm
  xrun stop -id myvm
  xrun list
  xrun snapshot -id myvm -dest /path/to/snapshot
  xrun restore -id myvm -source /path/to/snapshot`)
	return nil
}

func handleCreate(ctx context.Context, manager *sandbox.Manager, args []string) error {
	log.Info("Creating sandbox with args: %v", args)

	fs := flag.NewFlagSet("create", flag.ContinueOnError)

	var flags CreateFlags
	fs.StringVar(&flags.ID, "id", "", "Sandbox identifier (required)")
	fs.StringVar(&flags.Image, "image", "", "OCI image reference containing kernel and initrd (required)")
	fs.StringVar(&flags.RootFS, "rootfs", "", "Path to root filesystem disk image (optional)")
	fs.UintVar(&flags.VCPUs, "vcpus", 1, "Number of vCPUs")
	fs.UintVar(&flags.Memory, "memory", 512, "Memory size in MB")
	fs.StringVar(&flags.MemBackend, "mem-backend", "anonymous", "Memory backend type: anonymous, file")
	fs.StringVar(&flags.MemFile, "mem-file", "", "Path to memory backend file (when mem-backend=file)")
	fs.StringVar(&flags.Cmdline, "cmdline", "console=hvc0 root=/dev/vda1 rw", "Kernel command line")
	fs.StringVar(&flags.VMM, "vmm", "cloud-hypervisor", "VMM driver to use")
	fs.BoolVar(&flags.AutoStart, "start", false, "Auto-start the VM after creation")

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Validate required flags
	if flags.ID == "" {
		return fmt.Errorf("-id is required")
	}
	if flags.Image == "" {
		return fmt.Errorf("-image is required")
	}

	log.Info("Creating sandbox %s with image %s", flags.ID, flags.Image)

	// Build options
	opts := sandbox.CreateOptions{
		VMM:       flags.VMM,
		VCPUs:     uint32(flags.VCPUs),
		AutoStart: flags.AutoStart,
		Image:     flags.Image,
		Memory: vmm.MemoryConfig{
			SizeMB:      uint32(flags.Memory),
			Backend:     vmm.MemoryBackendType(flags.MemBackend),
			BackendPath: flags.MemFile,
		},
		Boot: vmm.BootConfig{
			Cmdline: flags.Cmdline,
		},
	}

	// Build rootfs disk config if provided
	if flags.RootFS != "" {
		opts.RootFS = vmm.DiskConfig{
			Path:     flags.RootFS,
			ReadOnly: false,
			Format:   "raw",
		}
	}

	if err := manager.Create(ctx, flags.ID, opts); err != nil {
		log.Error("Failed to create sandbox %s: %v", flags.ID, err)
		return err
	}

	log.Info("Successfully created sandbox %s", flags.ID)
	fmt.Printf("Created sandbox %s\n", flags.ID)
	return nil
}

func handleStart(ctx context.Context, manager *sandbox.Manager, args []string) error {
	log.Info("Starting sandbox with args: %v", args)

	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	var id string
	fs.StringVar(&id, "id", "", "Sandbox identifier (required)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if id == "" {
		return fmt.Errorf("-id is required")
	}

	log.Info("Loading VM for sandbox %s", id)

	vm, err := manager.Get(ctx, id)
	if err != nil {
		log.Error("Failed to get sandbox %s: %v", id, err)
		return fmt.Errorf("failed to load sandbox %s: %w", id, err)
	}

	log.Info("Starting VM for sandbox %s", id)

	if err := vm.Start(ctx); err != nil {
		log.Error("Failed to start VM for sandbox %s: %v", id, err)
		return err
	}

	// Update state in storage
	if err := manager.UpdateSandboxState(id, vmm.VMStateRunning); err != nil {
		fmt.Printf("Warning: failed to update sandbox state: %v\n", err)
	}

	// Get PID from VM and save to storage
	if info, err := vm.Info(ctx); err == nil {
		if info.PID > 0 {
			if err := manager.UpdateSandboxPID(id, info.PID); err != nil {
				fmt.Printf("Warning: failed to update sandbox PID: %v\n", err)
			}
		}
	}

	log.Info("Successfully started sandbox %s", id)
	fmt.Printf("Started sandbox %s\n", id)
	return nil
}

func handleStop(ctx context.Context, manager *sandbox.Manager, args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	var id string
	var force bool
	fs.StringVar(&id, "id", "", "Sandbox identifier (required)")
	fs.BoolVar(&force, "force", false, "Force stop")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if id == "" {
		return fmt.Errorf("-id is required")
	}

	if err := manager.Stop(ctx, id, force); err != nil {
		return err
	}

	// Update state in storage
	if err := manager.UpdateSandboxState(id, vmm.VMStateStopped); err != nil {
		fmt.Printf("Warning: failed to update sandbox state: %v\n", err)
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
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	var id, dest string
	var compress bool
	fs.StringVar(&id, "id", "", "Sandbox identifier (required)")
	fs.StringVar(&dest, "dest", "", "Snapshot destination path (required)")
	fs.BoolVar(&compress, "compress", false, "Compress snapshot")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if id == "" || dest == "" {
		return fmt.Errorf("-id and -dest are required")
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
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	var id, source string
	fs.StringVar(&id, "id", "", "Sandbox identifier (required)")
	fs.StringVar(&source, "source", "", "Snapshot source path (required)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if id == "" || source == "" {
		return fmt.Errorf("-id and -source are required")
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
	fs := flag.NewFlagSet("pause", flag.ContinueOnError)
	var id string
	fs.StringVar(&id, "id", "", "Sandbox identifier (required)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if id == "" {
		return fmt.Errorf("-id is required")
	}

	if err := manager.Pause(ctx, id); err != nil {
		return err
	}

	fmt.Printf("Paused sandbox %s\n", id)
	return nil
}

func handleResume(ctx context.Context, manager *sandbox.Manager, args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	var id string
	fs.StringVar(&id, "id", "", "Sandbox identifier (required)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if id == "" {
		return fmt.Errorf("-id is required")
	}

	if err := manager.Resume(ctx, id); err != nil {
		return err
	}

	fmt.Printf("Resumed sandbox %s\n", id)
	return nil
}

func handleDelete(ctx context.Context, manager *sandbox.Manager, args []string) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	var id string
	var force bool
	fs.StringVar(&id, "id", "", "Sandbox identifier (required)")
	fs.BoolVar(&force, "force", false, "Force delete")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if id == "" {
		return fmt.Errorf("-id is required")
	}

	if err := manager.Delete(ctx, id, force); err != nil {
		return err
	}

	fmt.Printf("Deleted sandbox %s\n", id)
	return nil
}
