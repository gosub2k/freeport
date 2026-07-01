package controller

import (
	"context"
	"fmt"
	"sync"

	"freeport/pkg/util"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const DriverName = "freeport"

type volumeRecord struct {
	id           string
	capacityBytes int64
	storageClass  string
	topology      *csi.Topology
	context       map[string]string
}

type ControllerServer struct {
	csi.UnimplementedControllerServer
	mu     sync.Mutex
	byName map[string]*volumeRecord // idempotency key
	byID   map[string]*volumeRecord // for ValidateVolumeCapabilities / DeleteVolume
}

func NewControllerServer() *ControllerServer {
	return &ControllerServer{
		byName: make(map[string]*volumeRecord),
		byID:   make(map[string]*volumeRecord),
	}
}

// CreateVolume receives the topology selected by the scheduler and echoes it back.
func (cs *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	util.Log.Info("CreateVolume", "name", req.Name, "capacity", req.CapacityRange)

	if len(req.Name) == 0 {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if req.CapacityRange == nil || req.CapacityRange.RequiredBytes <= 0 {
		return nil, status.Error(codes.InvalidArgument, "capacity is required")
	}

	capacityBytes := req.CapacityRange.GetRequiredBytes()

	cs.mu.Lock()
	defer cs.mu.Unlock()

	if existing, ok := cs.byName[req.Name]; ok {
		if existing.capacityBytes != capacityBytes {
			return nil, status.Errorf(codes.AlreadyExists,
				"volume %q already exists with capacity %d, requested %d",
				req.Name, existing.capacityBytes, capacityBytes)
		}
		// Idempotent: same name + same capacity → return the existing volume.
		util.Log.Info("CreateVolume idempotent", "id", existing.id)
		return cs.volumeResponse(existing), nil
	}

	volumeID := fmt.Sprintf("vol-%s-%s", DriverName, req.Name)

	var selectedTopology *csi.Topology
	if reqs := req.GetAccessibilityRequirements(); reqs != nil && len(reqs.GetRequisite()) > 0 {
		selectedTopology = reqs.GetRequisite()[0]
	}

	storageClass := req.Parameters["storageClassName"]

	rec := &volumeRecord{
		id:           volumeID,
		capacityBytes: capacityBytes,
		storageClass:  storageClass,
		topology:      selectedTopology,
		context:       map[string]string{"storageClassName": storageClass},
	}
	cs.byName[req.Name] = rec
	cs.byID[volumeID] = rec

	util.Log.Info("CreateVolume created", "id", volumeID)
	return cs.volumeResponse(rec), nil
}

func (cs *ControllerServer) volumeResponse(rec *volumeRecord) *csi.CreateVolumeResponse {
	var topologies []*csi.Topology
	if rec.topology != nil {
		topologies = []*csi.Topology{rec.topology}
	}
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:           rec.id,
			CapacityBytes:      rec.capacityBytes,
			AccessibleTopology: topologies,
			VolumeContext:      rec.context,
		},
	}
}

func (cs *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	util.Log.Info("DeleteVolume", "volumeID", req.VolumeId)

	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Find by ID and remove from both maps. Not-found is idempotent per CSI spec.
	if rec, ok := cs.byID[req.VolumeId]; ok {
		for name, r := range cs.byName {
			if r == rec {
				delete(cs.byName, name)
				break
			}
		}
		delete(cs.byID, req.VolumeId)
	}

	return &csi.DeleteVolumeResponse{}, nil
}

// ValidateVolumeCapabilities is mandatory in the CSI spec regardless of which
// optional features the controller implements.
func (cs *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	util.Log.Info("ValidateVolumeCapabilities", "volumeID", req.VolumeId)

	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	if _, ok := cs.byID[req.VolumeId]; !ok {
		return nil, status.Errorf(codes.NotFound, "volume %q not found", req.VolumeId)
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.VolumeCapabilities,
		},
	}, nil
}

// ControllerGetCapabilities advertises what this controller implements.
func (cs *ControllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	util.Log.Info("ControllerGetCapabilities called")
	cap := func(t csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
		return &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: t},
			},
		}
	}
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			cap(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
		},
	}, nil
}

// ControllerPublishVolume / ControllerUnpublishVolume — not needed for local
// storage (attachRequired: false in the CSIDriver object).
func (cs *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	return &csi.ControllerPublishVolumeResponse{}, nil
}

func (cs *ControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}
