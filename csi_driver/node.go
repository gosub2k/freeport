package main

import (
	"context"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"os"
)

type NodeServer struct {
	csi.UnimplementedNodeServer
	nodeID string
}

func NewNodeServer(nodeID string) *NodeServer {
	return &NodeServer{nodeID: nodeID}
}

func (ns *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	log.Debug("Got request: %v for %v", req, req.TargetPath)
	log.Info("NodePublishVolume: %v", req.TargetPath)
	targetPath := req.GetTargetPath()
	if err := os.MkdirAll(targetPath, 0755); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to create target path: %v", err)
	}
	// For hostpath/local: nothing to mount; just ensure dir exists
	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	log.Info("NodeUNPublicshVolume: %v", req.GetTargetPath())
	targetPath := req.GetTargetPath()
	os.RemoveAll(targetPath)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: ns.nodeID}, nil
}

func (ns *NodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil // No staging needed
}
