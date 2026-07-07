package driver

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"freeport/pkg/devicescan"
	"freeport/pkg/util"
)

type NodeServer struct {
	csi.UnimplementedNodeServer
	hostRoot   string
	nodeID     string
	driverName string
	scanFn     func(hostRoot string) []hostBlockDevice
	mountFn    func(source, target, fstype string, flags uintptr, data string) error
}

func NewNodeServer(nodeID, hostRoot, driverName string, opts ...func(*NodeServer)) *NodeServer {
	util.Log.Info("NewNodeServer", "nodeID", nodeID)
	ns := &NodeServer{
		nodeID:     nodeID,
		hostRoot:   hostRoot,
		driverName: driverName,
		scanFn:     scanReadyDevices,
		mountFn:    syscall.Mount,
	}
	for _, opt := range opts {
		opt(ns)
	}
	return ns
}

// WithNoScan replaces USB device scanning with a no-op. Use in tests where no
// real USB hardware is present and sysfs/devfs access should be avoided.
func WithNoScan() func(*NodeServer) {
	return func(ns *NodeServer) {
		ns.scanFn = func(string) []hostBlockDevice { return nil }
	}
}

// WithFakeDevice replaces the scanner with one that returns a single synthetic
// device whose mountpoint is mountpointDir. Use in tests together with
// WithNoMount to exercise the full publish/unpublish flow without hardware.
func WithFakeDevice(mountpointDir string) func(*NodeServer) {
	return func(ns *NodeServer) {
		ns.scanFn = func(string) []hostBlockDevice {
			return []hostBlockDevice{{
				serial:       "test-serial",
				manufacturer: "Test Co",
				model:        "Test USB",
				partition:    1,
				mountpoint:   mountpointDir,
			}}
		}
	}
}

// WithNoMount replaces syscall.Mount with a no-op. Use in tests running without
// CAP_SYS_ADMIN where bind mounts are not available.
func WithNoMount() func(*NodeServer) {
	return func(ns *NodeServer) {
		ns.mountFn = func(source, target, fstype string, flags uintptr, data string) error {
			return nil
		}
	}
}

func (ns *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	targetPath := filepath.Join(ns.hostRoot, req.GetTargetPath())
	storageClass, storageClassIsSet := req.VolumeContext["storageClassName"]
	util.Log.Info("NodePublishVolume", "node", ns.nodeID, "volumeID", volumeID, "targetPath", targetPath, "storageClass", storageClass)

	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	if !storageClassIsSet {
		return nil, status.Error(codes.InvalidArgument, "storageClassName is required in the volumeContext")
	}

	scanned := ns.scanFn(ns.hostRoot)
	util.Log.Info("scanned devices", "total", len(scanned))

	candidates := matchVolumeContext(scanned, ns.driverName, req.VolumeContext)
	if len(candidates) == 0 {
		util.Log.Error("no matching block devices on node", "node", ns.nodeID)
		return nil, status.Errorf(codes.ResourceExhausted, "no matching block devices on node %s", ns.nodeID)
	}
	util.Log.Info("matched devices", "count", len(candidates))

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].serial < candidates[j].serial
	})
	dev := candidates[0]
	util.Log.Info("selected device", "serial", dev.serial, "mountpoint", dev.mountpoint)

	volPath := filepath.Join(ns.hostRoot, dev.mountpoint, volumeID)
	util.Log.Info("ensuring volume directory exists", "path", volPath)
	if err := os.MkdirAll(volPath, 0750); err != nil {
		util.Log.Error("failed to create volume directory", "path", volPath, "err", err)
		return nil, status.Errorf(codes.Internal, "failed to create volume directory %s: %v", volPath, err)
	}

	util.Log.Info("ensuring target path exists", "path", targetPath)
	if err := os.MkdirAll(targetPath, 0750); err != nil {
		util.Log.Error("failed to create target path", "path", targetPath, "err", err)
		return nil, status.Errorf(codes.Internal, "failed to create target path %s: %v", targetPath, err)
	}

	util.Log.Info("bind mounting", "source", volPath, "target", targetPath)
	if err := ns.mountFn(volPath, targetPath, "", syscall.MS_BIND, ""); err != nil {
		if err == syscall.EBUSY {
			util.Log.Info("target already bind-mounted, returning idempotent success", "target", targetPath)
			return &csi.NodePublishVolumeResponse{}, nil
		}
		util.Log.Error("bind mount failed", "source", volPath, "target", targetPath, "err", err)
		return nil, status.Errorf(codes.Internal, "bind mount %s -> %s failed: %v", volPath, targetPath, err)
	}

	util.Log.Info("NodePublishVolume succeeded", "volumeID", volumeID, "volPath", volPath, "targetPath", targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()

	util.Log.Info("NodeUnpublishVolume", "node", ns.nodeID, "volumeID", volumeID, "targetPath", targetPath)

	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}

	targetPath = filepath.Join(ns.hostRoot, req.GetTargetPath())
	util.Log.Info("unmounting target path", "path", targetPath)
	if err := syscall.Unmount(targetPath, 0); err != nil {
		if err == syscall.ENOENT || err == syscall.EINVAL || err == syscall.EPERM {
			// ENOENT/EINVAL: not a mount point — nothing to unmount.
			// EPERM: caller lacks CAP_SYS_ADMIN (e.g. unprivileged test); the
			// bind-mount either never happened or will be cleaned up by the
			// kubelet. Either way, proceed to remove the directory.
			util.Log.Info("target already unmounted or not a mount point, ignoring", "path", targetPath)
		} else {
			util.Log.Error("unmount failed", "path", targetPath, "err", err)
			return nil, status.Errorf(codes.Internal, "unmount %s failed: %v", targetPath, err)
		}
	}
	pathToRm := targetPath
	util.Log.Info("removing target path", "path", pathToRm)
	if err := os.RemoveAll(pathToRm); err != nil {
		util.Log.Error("remove failed", "path", pathToRm, "err", err)
		return nil, status.Errorf(codes.Internal, "'remove' (rename) %s failed: %v", pathToRm, err)
	}

	util.Log.Info("NodeUnpublishVolume succeeded", "volumeID", volumeID, "targetPath", targetPath)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	util.Log.Debug("NodeGetInfo", "nodeID", ns.nodeID)

	devices := ns.scanFn(ns.hostRoot)
	segments := map[string]string{}
	for _, dev := range devices {
		segments[ns.driverName+"/"+devicescan.DeviceClassKey(dev.manufacturer, dev.model)] = "true"
	}

	for _, dev := range devices {
		util.Log.Info(dev.String())
	}
	resp := &csi.NodeGetInfoResponse{NodeId: ns.nodeID}
	if len(segments) > 0 {
		resp.AccessibleTopology = &csi.Topology{Segments: segments}
	}

	return resp, nil
}

func (ns *NodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	util.Log.Debug("NodeGetCapabilities")
	return &csi.NodeGetCapabilitiesResponse{}, nil
}
