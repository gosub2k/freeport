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

	"freeport/pkg/util"
)

type NodeServer struct {
	csi.UnimplementedNodeServer
	nodeID string
}

func NewNodeServer(nodeID string) *NodeServer {
	util.Log.Info("NewNodeServer", "nodeID", nodeID)
	return &NodeServer{nodeID: nodeID}
}

func (ns *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	targetPath := filepath.Join("/host", req.GetTargetPath())
	storageClass := ""
	storageClass, _ = req.VolumeContext["storageClassName"]
	util.Log.Info("NodePublishVolume", "node", ns.nodeID, "volumeID", volumeID, "targetPath", targetPath, "storageClass", storageClass)

	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	if storageClass == "" {
		return nil, status.Error(codes.InvalidArgument, "storageClassName is required in the volumeContext")
	}

	devices, err := util.GetAvailableDevices(ctx, storageClass)
	if err != nil {
		util.Log.Error("failed to get available devices", "err", err)
		return nil, status.Errorf(codes.Internal, "failed to get available devices: %v", err)
	}
	util.Log.Info("fetched available devices", "total", len(devices))

	var nodeDevices []util.BlockDevice
	for _, d := range devices {
		if d.Node == ns.nodeID {
			nodeDevices = append(nodeDevices, d)
		}
	}

	if len(nodeDevices) == 0 {
		util.Log.Error("no block devices available on node", "node", ns.nodeID)
		return nil, status.Errorf(codes.ResourceExhausted, "no block devices available on node %s", ns.nodeID)
	}
	util.Log.Info("devices on this node", "count", len(nodeDevices))

	sort.Slice(nodeDevices, func(i, j int) bool {
		return nodeDevices[i].Name < nodeDevices[j].Name
	})
	dev := nodeDevices[0]
	util.Log.Info("selected device", "name", dev.Name, "mountpoint", dev.MountPoint)

	volPath := filepath.Join("/host", dev.MountPoint, volumeID)
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
	if err := syscall.Mount(volPath, targetPath, "", syscall.MS_BIND, ""); err != nil {
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
	targetPath := filepath.Join("/host", req.GetTargetPath())

	util.Log.Info("NodeUnpublishVolume", "node", ns.nodeID, "volumeID", volumeID, "targetPath", targetPath)

	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	util.Log.Info("unmounting target path", "path", targetPath)
	if err := syscall.Unmount(targetPath, 0); err != nil {
		if err == syscall.ENOENT || err == syscall.EINVAL {
			util.Log.Info("target already unmounted or not a mount point, returning idempotent success", "path", targetPath)
			return &csi.NodeUnpublishVolumeResponse{}, nil
		}
		util.Log.Error("unmount failed", "path", targetPath, "err", err)
		return nil, status.Errorf(codes.Internal, "unmount %s failed: %v", targetPath, err)
	}

	util.Log.Info("NodeUnpublishVolume succeeded", "volumeID", volumeID, "targetPath", targetPath)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	util.Log.Debug("NodeGetInfo", "nodeID", ns.nodeID)
	return &csi.NodeGetInfoResponse{NodeId: ns.nodeID}, nil
}

func (ns *NodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	util.Log.Debug("NodeGetCapabilities")
	return &csi.NodeGetCapabilitiesResponse{}, nil
}
