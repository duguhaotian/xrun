// Package daemon provides the xrund gRPC server implementation.
package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	pb "github.com/microvm/sandbox/api/proto"
	"github.com/microvm/sandbox/internal/config"
	"github.com/microvm/sandbox/pkg/image"
	"github.com/microvm/sandbox/pkg/log"
	"github.com/microvm/sandbox/pkg/network"
	"github.com/microvm/sandbox/pkg/sandbox"
	"github.com/microvm/sandbox/pkg/storage"
	"github.com/microvm/sandbox/pkg/vmm"
	"github.com/microvm/sandbox/pkg/vmm/cloudhypervisor"
)

// Server implements the SandboxService gRPC server.
type Server struct {
	pb.UnimplementedSandboxServiceServer

	config     *config.Config
	grpcServer *grpc.Server
	store      *sandbox.Store
	vmmFactory *vmm.Factory
	storageMgr *storage.Manager
	imageCache *image.Cache
	netMgr     *network.Manager
	containerd *containerd.Client

	// Runtime state
	sandboxes map[string]*SandboxRuntime
	mu        sync.RWMutex

	// Shutdown state
	shutdownCh chan struct{}
}

// SandboxRuntime holds runtime state for a sandbox.
type SandboxRuntime struct {
	Meta     *sandbox.Meta
	VM       vmm.VM
	Cancel   context.CancelFunc
	NetNS    *network.NetNS
	ImageRef string
}

// NewServer creates a new daemon server.
func NewServer(cfg *config.Config) (*Server, error) {
	// Create store
	store := sandbox.NewStore(cfg.DataDir)

	// Create VMM factory
	factory := vmm.NewFactory()
	chDriver := cloudhypervisor.NewDriver(cfg.VMM.CHBinaryPath, cfg.DataDir)
	factory.Register("cloud-hypervisor", chDriver)

	return &Server{
		config:     cfg,
		store:      store,
		vmmFactory: factory,
		sandboxes:  make(map[string]*SandboxRuntime),
		shutdownCh: make(chan struct{}),
	}, nil
}

// Init initializes the server.
func (s *Server) Init(ctx context.Context) error {
	// Connect to containerd
	client, err := containerd.New(s.config.ContainerdAddress)
	if err != nil {
		return fmt.Errorf("failed to connect to containerd: %w", err)
	}
	s.containerd = client

	// Get snapshotter
	snapshotter := client.SnapshotService(s.config.Snapshotter)

	// Initialize storage manager
	s.storageMgr = storage.NewManager(snapshotter, s.config.SnapshotNamespace, s.config.DataDir)
	if err := s.storageMgr.Init(ctx); err != nil {
		return fmt.Errorf("failed to initialize storage manager: %w", err)
	}

	// Initialize image cache
	s.imageCache = image.NewCache(client, snapshotter, s.config.SnapshotNamespace, s.config.DataDir)
	if err := s.imageCache.Init(ctx); err != nil {
		return fmt.Errorf("failed to initialize image cache: %w", err)
	}

	// Initialize network manager
	s.netMgr = network.NewManager(network.Config{
		Enabled:       s.config.Network.Enabled,
		UsePool:       s.config.Network.UsePool,
		PoolSize:      s.config.Network.PoolSize,
		CNIPluginPath: s.config.Network.CNIPluginPath,
		CNIConfigPath: s.config.Network.CNIConfigPath,
		VMSubnet:      s.config.Network.VMSubnet,
		VMGateway:     s.config.Network.VMGateway,
		VMIPRange:     s.config.Network.VMIPRange,
	}, s.config.DataDir)

	if err := s.netMgr.Init(); err != nil {
		return fmt.Errorf("failed to initialize network manager: %w", err)
	}

	log.Info("Server initialized successfully")
	return nil
}

// Start starts the gRPC server.
func (s *Server) Start() error {
	// Remove existing socket
	os.Remove(s.config.SocketPath)

	// Create listener
	lis, err := net.Listen("unix", s.config.SocketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.config.SocketPath, err)
	}

	// Create gRPC server with keepalive settings
	s.grpcServer = grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
		}),
	)
	pb.RegisterSandboxServiceServer(s.grpcServer, s)
	reflection.Register(s.grpcServer)

	log.Info("xrund server starting on %s", s.config.SocketPath)

	// Handle signals in a separate goroutine
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		sig := <-sigCh
		log.Info("Received signal %v, initiating shutdown...", sig)
		signal.Stop(sigCh)
		close(sigCh)
		s.Shutdown()
	}()

	// Start serving in a goroutine so we can monitor shutdown
	go func() {
		if err := s.grpcServer.Serve(lis); err != nil {
			log.Error("gRPC server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	<-s.shutdownCh
	log.Info("Server shutdown complete")

	return nil
}

// Shutdown stops the server gracefully.
func (s *Server) Shutdown() {
	select {
	case <-s.shutdownCh:
		return
	default:
		close(s.shutdownCh)
	}

	log.Info("Shutting down server...")

	// Stop accepting new requests
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
		log.Info("gRPC server stopped")
	}

	// Stop all running sandboxes
	s.stopAllSandboxes()

	log.Info("Server shutdown finished")
}

// stopAllSandboxes stops all running sandboxes during shutdown.
func (s *Server) stopAllSandboxes() {
	s.mu.Lock()
	runtimes := make([]*SandboxRuntime, 0, len(s.sandboxes))
	for _, runtime := range s.sandboxes {
		runtimes = append(runtimes, runtime)
	}
	s.mu.Unlock()

	if len(runtimes) == 0 {
		return
	}

	log.Info("Stopping %d running sandboxes...", len(runtimes))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i, runtime := range runtimes {
		log.Info("Stopping sandbox %s (%d/%d)", runtime.Meta.ID, i+1, len(runtimes))
		if err := runtime.VM.Stop(ctx); err != nil {
			log.Warn("Failed to stop sandbox %s: %v, forcing...", runtime.Meta.ID, err)
			runtime.VM.ForceStop(ctx)
		}
		s.imageCache.Release(runtime.ImageRef)
	}

	s.mu.Lock()
	for _, runtime := range runtimes {
		delete(s.sandboxes, runtime.Meta.ID)
	}
	s.mu.Unlock()

	log.Info("All sandboxes stopped")
}

// Run implements the Run RPC.
func (s *Server) Run(ctx context.Context, req *pb.RunRequest) (*pb.RunResponse, error) {
	log.Info("Run request: id=%s, image=%s", req.Id, req.Image)

	// Check for shutdown
	select {
	case <-s.shutdownCh:
		return nil, fmt.Errorf("server is shutting down")
	default:
	}

	// Add timeout to prevent indefinite blocking
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// Check if sandbox already exists
	if _, err := s.store.Load(req.Id); err == nil {
		return nil, fmt.Errorf("sandbox %s already exists", req.Id)
	}

	// Get VMM driver
	driver, ok := s.vmmFactory.Get(s.config.VMM.DefaultDriver)
	if !ok {
		return nil, fmt.Errorf("VMM driver %s not found", s.config.VMM.DefaultDriver)
	}

	// Check for shutdown before proceeding
	select {
	case <-s.shutdownCh:
		return nil, fmt.Errorf("server is shutting down")
	case <-ctx.Done():
		return nil, fmt.Errorf("operation timed out: %w", ctx.Err())
	default:
	}

	// 1. Get or mount image (View snapshot, shared)
	imgMount, err := s.imageCache.GetOrMount(ctx, req.Image)
	if err != nil {
		return nil, fmt.Errorf("failed to mount image: %w", err)
	}

	// 2. Setup network
	vmNet, err := s.netMgr.Setup(ctx, req.Id)
	if err != nil {
		s.imageCache.Release(req.Image)
		return nil, fmt.Errorf("failed to setup network: %w", err)
	}

	// 3. Create memory file (RW snapshot)
	memFile, err := s.storageMgr.CreateMemoryFile(ctx, req.Id, req.MemoryMb)
	if err != nil {
		s.netMgr.Cleanup(ctx, req.Id, vmNet.NetNS)
		s.imageCache.Release(req.Image)
		return nil, fmt.Errorf("failed to create memory file: %w", err)
	}

	// 4. Create VM config
	vmConfig := vmm.VMConfig{
		ID:    req.Id,
		VCPUs: req.Vcpus,
		Memory: vmm.MemoryConfig{
			SizeMB:      req.MemoryMb,
			Backend:     vmm.MemoryBackendFile,
			BackendPath: memFile.Path,
			Shared:      false,
		},
		Boot: vmm.BootConfig{
			KernelPath: imgMount.KernelPath,
			InitrdPath: imgMount.InitrdPath,
			Cmdline:    req.Cmdline,
		},
		RootFS:    req.Rootfs,
		TapDevice: vmNet.TapDevice,
		VMIP:      vmNet.VMIP,
	}
	if vmNet.NetNS != nil {
		vmConfig.NetNS = vmNet.NetNS.Path
	}

	// 5. Create VM
	vm, err := driver.Create(ctx, vmConfig)
	if err != nil {
		vmDir := fmt.Sprintf("%s/vms/%s", s.config.DataDir, req.Id)
		os.RemoveAll(vmDir)
		s.storageMgr.DeleteMemoryFile(ctx, memFile.SnapshotKey)
		s.netMgr.Cleanup(ctx, req.Id, vmNet.NetNS)
		s.imageCache.Release(req.Image)
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	// 6. Start VM
	if err := vm.Start(ctx); err != nil {
		vm.ForceStop(ctx)
		s.storageMgr.DeleteMemoryFile(ctx, memFile.SnapshotKey)
		s.netMgr.Cleanup(ctx, req.Id, vmNet.NetNS)
		s.imageCache.Release(req.Image)
		vmDir := fmt.Sprintf("%s/vms/%s", s.config.DataDir, req.Id)
		os.RemoveAll(vmDir)
		return nil, fmt.Errorf("failed to start VM: %w", err)
	}

	// Get VM info
	vmInfo, err := vm.Info(ctx)
	if err != nil {
		log.Warn("Failed to get VM info: %v", err)
	}

	// 7. Save metadata
	meta := sandbox.CreateMeta(req.Id, s.config.VMM.DefaultDriver, req.Vcpus, req.MemoryMb)
	meta.Cmdline = req.Cmdline
	meta.State = vmm.VMStateRunning
	meta.PID = vmInfo.PID
	meta.MemorySnapshot = memFile.SnapshotKey
	meta.Labels = req.Labels

	// Set image info
	meta.SetImageInfo(req.Image, imgMount.MountPath, req.Rootfs, imgMount.KernelPath, imgMount.InitrdPath)

	// Set network info (safely handle nil NetNS)
	if vmNet.NetNS != nil {
		meta.SetNetworkInfo(vmNet.NetNS.Path, vmNet.TapDevice, vmNet.HostIP, true)
	} else {
		meta.SetNetworkInfo("", vmNet.TapDevice, vmNet.HostIP, false)
	}

	if err := s.store.Save(meta); err != nil {
		vm.ForceStop(ctx)
		s.storageMgr.DeleteMemoryFile(ctx, memFile.SnapshotKey)
		s.netMgr.Cleanup(ctx, req.Id, vmNet.NetNS)
		s.imageCache.Release(req.Image)
		vmDir := fmt.Sprintf("%s/vms/%s", s.config.DataDir, req.Id)
		os.RemoveAll(vmDir)
		return nil, fmt.Errorf("failed to save metadata: %w", err)
	}

	// 8. Store runtime
	s.mu.Lock()
	s.sandboxes[req.Id] = &SandboxRuntime{
		Meta:     meta,
		VM:       vm,
		NetNS:    vmNet.NetNS,
		ImageRef: req.Image,
	}
	s.mu.Unlock()

	log.Info("Sandbox %s started successfully (PID: %d)", req.Id, vmInfo.PID)

	return &pb.RunResponse{
		Id:        req.Id,
		State:     string(vmm.VMStateRunning),
		Pid:       int32(vmInfo.PID),
		IpAddress: vmNet.HostIP,
	}, nil
}

// Stop implements the Stop RPC.
func (s *Server) Stop(ctx context.Context, req *pb.StopRequest) (*pb.StopResponse, error) {
	log.Info("Stop request: id=%s, force=%v", req.Id, req.Force)

	// Check for shutdown
	select {
	case <-s.shutdownCh:
		return nil, fmt.Errorf("server is shutting down")
	default:
	}

	// First check if sandbox exists and is running
	log.Debug("[Stop] Loading sandbox %s from store", req.Id)
	meta, err := s.store.Load(req.Id)
	if err != nil {
		log.Error("[Stop] Failed to load sandbox %s: %v", req.Id, err)
		return nil, fmt.Errorf("sandbox %s not found", req.Id)
	}
	log.Debug("[Stop] Sandbox %s loaded, state=%s", req.Id, meta.State)

	if meta.State != vmm.VMStateRunning {
		log.Warn("[Stop] Sandbox %s is not running (state=%s)", req.Id, meta.State)
		return nil, fmt.Errorf("sandbox %s is not running", req.Id)
	}

	log.Debug("[Stop] Acquiring runtime lock for sandbox %s", req.Id)
	s.mu.Lock()
	runtime, ok := s.sandboxes[req.Id]
	s.mu.Unlock()
	log.Debug("[Stop] Runtime lock released, sandbox %s in cache=%v", req.Id, ok)

	if !ok {
		// Sandbox is marked as running in store but not in runtime cache
		// This can happen if the daemon was restarted
		// Just update the state to stopped
		log.Warn("[Stop] Sandbox %s is marked as running but not in runtime cache, updating state to stopped", req.Id)
		if err := s.store.UpdateState(req.Id, vmm.VMStateStopped); err != nil {
			return nil, fmt.Errorf("failed to update sandbox state: %w", err)
		}
		return &pb.StopResponse{
			Id:    req.Id,
			State: string(vmm.VMStateStopped),
		}, nil
	}

	// Stop VM
	log.Info("[Stop] Stopping VM for sandbox %s (force=%v)", req.Id, req.Force)
	var stopErr error
	if req.Force {
		log.Debug("[Stop] Calling ForceStop for sandbox %s", req.Id)
		stopErr = runtime.VM.ForceStop(ctx)
	} else {
		log.Debug("[Stop] Calling Stop for sandbox %s", req.Id)
		stopErr = runtime.VM.Stop(ctx)
	}
	log.Info("[Stop] VM stop completed for sandbox %s, err=%v", req.Id, stopErr)

	if stopErr != nil {
		return nil, fmt.Errorf("failed to stop VM: %w", stopErr)
	}

	// Update metadata
	log.Debug("[Stop] Updating state to stopped for sandbox %s", req.Id)
	if err := s.store.UpdateState(req.Id, vmm.VMStateStopped); err != nil {
		log.Warn("[Stop] Failed to update state: %v", err)
	}

	// Cleanup network
	log.Debug("[Stop] Cleaning up network for sandbox %s", req.Id)
	if err := s.netMgr.Cleanup(ctx, req.Id, runtime.NetNS); err != nil {
		log.Warn("[Stop] Failed to cleanup network: %v", err)
	}

	// Release image
	log.Debug("[Stop] Releasing image for sandbox %s", req.Id)
	s.imageCache.Release(runtime.ImageRef)

	// Remove from runtime
	log.Debug("[Stop] Removing sandbox %s from runtime cache", req.Id)
	s.mu.Lock()
	delete(s.sandboxes, req.Id)
	s.mu.Unlock()

	log.Info("[Stop] Sandbox %s stopped successfully", req.Id)
	return &pb.StopResponse{
		Id:    req.Id,
		State: string(vmm.VMStateStopped),
	}, nil
}

// Delete implements the Delete RPC.
func (s *Server) Delete(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
	log.Info("Delete request: id=%s, force=%v", req.Id, req.Force)

	meta, err := s.store.Load(req.Id)
	if err != nil {
		return nil, err
	}

	// Check if running
	if meta.State == vmm.VMStateRunning {
		if !req.Force {
			return nil, fmt.Errorf("sandbox is running, stop it first or use --force")
		}
		// Force stop
		if _, err := s.Stop(ctx, &pb.StopRequest{Id: req.Id, Force: true}); err != nil {
			log.Warn("Failed to stop sandbox during delete: %v", err)
		}
	}

	// Delete memory file
	if meta.MemorySnapshot != "" {
		ctx := namespaces.WithNamespace(ctx, s.config.SnapshotNamespace)
		if err := s.storageMgr.DeleteMemoryFile(ctx, meta.MemorySnapshot); err != nil {
			log.Warn("Failed to delete memory file: %v", err)
		}
	}

	// Delete metadata
	if err := s.store.Delete(req.Id); err != nil {
		return nil, fmt.Errorf("failed to delete metadata: %w", err)
	}

	// Cleanup VM data directory
	vmDir := fmt.Sprintf("%s/vms/%s", s.config.DataDir, req.Id)
	if err := os.RemoveAll(vmDir); err != nil {
		log.Warn("Failed to remove VM directory %s: %v", vmDir, err)
	}

	// Cleanup snapshots directory
	snapshotsDir := fmt.Sprintf("%s/snapshots/%s", s.config.DataDir, req.Id)
	if err := os.RemoveAll(snapshotsDir); err != nil {
		log.Warn("Failed to remove snapshots directory %s: %v", snapshotsDir, err)
	}

	return &pb.DeleteResponse{Id: req.Id}, nil
}

// Get implements the Get RPC.
func (s *Server) Get(ctx context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	meta, err := s.store.Load(req.Id)
	if err != nil {
		return nil, err
	}

	return &pb.GetResponse{
		Sandbox: metaToProto(meta),
	}, nil
}

// List implements the List RPC.
func (s *Server) List(ctx context.Context, req *pb.ListRequest) (*pb.ListResponse, error) {
	metas, err := s.store.List()
	if err != nil {
		return nil, err
	}

	var sandboxes []*pb.Sandbox
	for _, meta := range metas {
		sandboxes = append(sandboxes, metaToProto(meta))
	}

	return &pb.ListResponse{
		Sandboxes: sandboxes,
	}, nil
}

// Snapshot implements the Snapshot RPC.
func (s *Server) Snapshot(ctx context.Context, req *pb.SnapshotRequest) (*pb.SnapshotResponse, error) {
	log.Info("Snapshot request: id=%s, name=%s", req.Id, req.Name)

	s.mu.Lock()
	runtime, ok := s.sandboxes[req.Id]
	s.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("sandbox %s not found or not running", req.Id)
	}

	// Pause VM for consistent snapshot
	if err := runtime.VM.Pause(ctx); err != nil {
		return nil, fmt.Errorf("failed to pause VM: %w", err)
	}

	// Create VM state snapshot via CH API
	vmStatePath := fmt.Sprintf("%s/vms/%s/snapshot-%s", s.config.DataDir, req.Id, req.Name)
	if err := runtime.VM.Snapshot(ctx, vmStatePath); err != nil {
		runtime.VM.Resume(ctx)
		return nil, fmt.Errorf("failed to create VM snapshot: %w", err)
	}

	// Commit memory snapshot
	ctx = namespaces.WithNamespace(ctx, s.config.SnapshotNamespace)
	snapshotKey, err := s.storageMgr.CreateSnapshot(ctx, req.Id, req.Name, vmStatePath)
	if err != nil {
		runtime.VM.Resume(ctx)
		return nil, fmt.Errorf("failed to create memory snapshot: %w", err)
	}

	// Resume VM
	if err := runtime.VM.Resume(ctx); err != nil {
		log.Warn("Failed to resume VM after snapshot: %v", err)
	}

	createdAt := time.Now().Format(time.RFC3339)

	return &pb.SnapshotResponse{
		SnapshotId: snapshotKey,
		CreatedAt:  createdAt,
	}, nil
}

// Restore implements the Restore RPC.
func (s *Server) Restore(ctx context.Context, req *pb.RestoreRequest) (*pb.RestoreResponse, error) {
	log.Info("Restore request: snapshot=%s, new_id=%s", req.SnapshotId, req.NewId)

	// Check if new ID already exists
	if _, err := s.store.Load(req.NewId); err == nil {
		return nil, fmt.Errorf("sandbox %s already exists", req.NewId)
	}

	// Restore memory snapshot
	ctx = namespaces.WithNamespace(ctx, s.config.SnapshotNamespace)
	memFile, err := s.storageMgr.RestoreSnapshot(ctx, req.SnapshotId, req.NewId)
	if err != nil {
		return nil, fmt.Errorf("failed to restore memory snapshot: %w", err)
	}

	// Get original snapshot info
	meta, err := s.store.Load(req.SnapshotId)
	if err != nil {
		// Try to get from snapshot metadata
		log.Warn("Could not load original metadata, using defaults")
		meta = sandbox.CreateMeta(req.SnapshotId, s.config.VMM.DefaultDriver, 1, 512)
		meta.Cmdline = "console=hvc0 root=/dev/vda1 rw"
	}

	// Get image reference from meta
	imageRef := meta.GetImageRef()

	// Get or mount image
	imgMount, err := s.imageCache.GetOrMount(ctx, imageRef)
	if err != nil {
		s.storageMgr.DeleteMemoryFile(ctx, memFile.SnapshotKey)
		return nil, fmt.Errorf("failed to mount image: %w", err)
	}

	// Setup network
	vmNet, err := s.netMgr.Setup(ctx, req.NewId)
	if err != nil {
		s.imageCache.Release(imageRef)
		s.storageMgr.DeleteMemoryFile(ctx, memFile.SnapshotKey)
		return nil, fmt.Errorf("failed to setup network: %w", err)
	}

	// Get VMM driver
	driver, ok := s.vmmFactory.Get(s.config.VMM.DefaultDriver)
	if !ok {
		return nil, fmt.Errorf("VMM driver not found")
	}

	// Create VM config (with shared=off for restore)
	vmConfig := vmm.VMConfig{
		ID:    req.NewId,
		VCPUs: meta.VCPUs,
		Memory: vmm.MemoryConfig{
			SizeMB:      meta.MemoryMB,
			Backend:     vmm.MemoryBackendFile,
			BackendPath: memFile.Path,
			Shared:      false, // Important: shared=off for restore
		},
		Boot: vmm.BootConfig{
			KernelPath: imgMount.KernelPath,
			InitrdPath: imgMount.InitrdPath,
			Cmdline:    meta.Cmdline,
		},
		RootFS:    meta.GetImageInfo().RootFS,
		TapDevice: vmNet.TapDevice,
		VMIP:      vmNet.VMIP,
	}
	if vmNet.NetNS != nil {
		vmConfig.NetNS = vmNet.NetNS.Path
	}

	// Create VM
	vm, err := driver.Create(ctx, vmConfig)
	if err != nil {
		s.netMgr.Cleanup(ctx, req.NewId, vmNet.NetNS)
		s.imageCache.Release(imageRef)
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	// Restore VM state
	vmStatePath := s.storageMgr.GetVMStatePath(req.SnapshotId)
	if err := vm.Restore(ctx, vmStatePath); err != nil {
		s.netMgr.Cleanup(ctx, req.NewId, vmNet.NetNS)
		s.imageCache.Release(imageRef)
		return nil, fmt.Errorf("failed to restore VM state: %w", err)
	}

	// Get VM info
	vmInfo, err := vm.Info(ctx)
	if err != nil {
		log.Warn("Failed to get VM info: %v", err)
	}

	// Save metadata
	newMeta := sandbox.CreateMeta(req.NewId, s.config.VMM.DefaultDriver, meta.VCPUs, meta.MemoryMB)
	newMeta.Cmdline = meta.Cmdline
	newMeta.State = vmm.VMStateRunning
	newMeta.PID = vmInfo.PID
	newMeta.MemorySnapshot = memFile.SnapshotKey

	// Set image info
	newMeta.SetImageInfo(imageRef, imgMount.MountPath, meta.GetImageInfo().RootFS, imgMount.KernelPath, imgMount.InitrdPath)

	// Set network info
	if vmNet.NetNS != nil {
		newMeta.SetNetworkInfo(vmNet.NetNS.Path, vmNet.TapDevice, vmNet.HostIP, true)
	} else {
		newMeta.SetNetworkInfo("", vmNet.TapDevice, vmNet.HostIP, false)
	}

	if err := s.store.Save(newMeta); err != nil {
		vm.ForceStop(ctx)
		s.netMgr.Cleanup(ctx, req.NewId, vmNet.NetNS)
		s.imageCache.Release(imageRef)
		return nil, fmt.Errorf("failed to save metadata: %w", err)
	}

	// Store runtime
	s.mu.Lock()
	s.sandboxes[req.NewId] = &SandboxRuntime{
		Meta:     newMeta,
		VM:       vm,
		NetNS:    vmNet.NetNS,
		ImageRef: imageRef,
	}
	s.mu.Unlock()

	return &pb.RestoreResponse{
		Id:    req.NewId,
		State: string(vmm.VMStateRunning),
	}, nil
}

// metaToProto converts Meta to protobuf Sandbox.
func metaToProto(meta *sandbox.Meta) *pb.Sandbox {
	return &pb.Sandbox{
		Id:        meta.ID,
		State:     string(meta.State),
		Pid:       int32(meta.PID),
		Vcpus:     meta.VCPUs,
		MemoryMb:  meta.MemoryMB,
		Image:     meta.GetImageRef(),
		Rootfs:    meta.GetImageInfo().RootFS,
		IpAddress: meta.GetIPAddr(),
		CreatedAt: meta.CreatedAt,
		Labels:    meta.Labels,
	}
}
