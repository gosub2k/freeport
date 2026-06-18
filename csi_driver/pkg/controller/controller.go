package controller

import (
	"freeport/pkg/util"

	"context"

	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	SocketPath = "unix:///socketdir/csi.sock"
	DriverName = "csi-local-minimal"
)

type ControllerServer struct {
	csi.UnimplementedControllerServer
}

// CreateVolume is the core logic for your request.
// It receives the topology selected by the K8s Scheduler and echoes it back.
func (cs *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	util.Log.Info("CreateVolume called: name=%s, capacity=%v", req.Name, req.CapacityRange)

	// 1. Validate
	if len(req.Name) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Name missing")
	}
	if req.CapacityRange == nil || req.CapacityRange.RequiredBytes <= 0 {
		return nil, status.Error(codes.InvalidArgument, "Capacity missing")
	}

	// 2. Generate a deterministic or random Volume ID
	// In a real driver, you would create the directory/block here.
	volumeID := fmt.Sprintf("vol-%s-%s", DriverName, req.Name)

	// 3. Extract Topology from Request
	// The K8s Scheduler puts the selected Node's topology in 'accessibility_requirements'.
	// For local storage with WaitForFirstConsumer, there should be exactly one 'requisite'
	// that matches the node the Pod was scheduled on.
	var selectedTopology *csi.Topology

	if req.GetAccessibilityRequirements() != nil && len(req.GetAccessibilityRequirements().GetRequisite()) > 0 {
		// We pick the first requisite. In a complex multi-zone scenario, you might need
		// logic to pick the best one, but for local storage, the scheduler picked the ONLY valid one.
		selectedTopology = req.GetAccessibilityRequirements().GetRequisite()[0]
		util.Log.Info("Scheduler selected topology: %v", selectedTopology.Segments)
	} else {
		// Fallback if no topology provided (shouldn't happen with WaitForFirstConsumer + Local)
		util.Log.Info("Warning: No accessibility requirements provided in request.")
	}

	// 4. Construct Response
	// CRITICAL: We return 'accessible_topology' matching the input.
	// The external-provisioner reads this and sets spec.nodeAffinity on the PV.
	resp := &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: req.CapacityRange.GetRequiredBytes(),
			// This is the magic field that binds the PV to the Node
			AccessibleTopology: []*csi.Topology{selectedTopology},
		},
	}

	util.Log.Info("Created volume %s with topology %v", volumeID, selectedTopology)
	return resp, nil
}

func (cs *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	util.Log.Info("DeleteVolume called: %s", req.VolumeId)
	// Add logic to delete directory/block here
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerGetCapabilities is required
func (cs *ControllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
					},
				},
			},
			// Add EXPAND_VOLUME if you support resizing
		},
	}, nil
}

// ControllerPublishVolume is NOT needed for local storage (attachRequired: false)
// But the RPC must exist. We can return unimplemented or empty success if attachRequired=false.
func (cs *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	// For local storage, we typically set attachRequired: false in the CSIDriver object.
	// If so, this is never called. If called, we can just return success.
	return &csi.ControllerPublishVolumeResponse{}, nil
}

func (cs *ControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}
