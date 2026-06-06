package main

import (
	"context"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ControllerServer struct {
	csi.UnimplementedControllerServer
	mu      sync.Mutex
	volumes map[string]*csi.Volume // volumeID -> volume
}

func NewControllerServer() *ControllerServer {
	return &ControllerServer{volumes: make(map[string]*csi.Volume)}
}

func (cs *ControllerServer) CreateVolume(_ context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name required")
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	// idempotent: return existing if name matches
	for _, v := range cs.volumes {
		if v.VolumeContext["name"] == req.GetName() {
			return &csi.CreateVolumeResponse{Volume: v}, nil
		}
	}
	vol := &csi.Volume{
		VolumeId:      req.GetName(),
		CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
		VolumeContext: map[string]string{"name": req.GetName()},
	}
	cs.volumes[vol.VolumeId] = vol
	return &csi.CreateVolumeResponse{Volume: vol}, nil
}

func (cs *ControllerServer) DeleteVolume(_ context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id required")
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	delete(cs.volumes, req.GetVolumeId())
	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *ControllerServer) ListVolumes(_ context.Context, _ *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var entries []*csi.ListVolumesResponse_Entry
	for _, v := range cs.volumes {
		entries = append(entries, &csi.ListVolumesResponse_Entry{Volume: v})
	}
	return &csi.ListVolumesResponse{Entries: entries}, nil
}

func (cs *ControllerServer) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id required")
	}
	cs.mu.Lock()
	_, ok := cs.volumes[req.GetVolumeId()]
	cs.mu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "volume %s not found", req.GetVolumeId())
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
	}, nil
}

func (cs *ControllerServer) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	caps := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
	}
	var rpcCaps []*csi.ControllerServiceCapability
	for _, c := range caps {
		rpcCaps = append(rpcCaps, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: c},
			},
		})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: rpcCaps}, nil
}
