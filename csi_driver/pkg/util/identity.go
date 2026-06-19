package util

import (
	"context"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type IdentityServer struct {
	csi.UnimplementedIdentityServer
	driverName   string
	version      string
	isController bool
}

func NewIdentityServer(name, version string, isController bool) *IdentityServer {
	Log.Info("NewIdentityServer", "name", name, "version", version)
	return &IdentityServer{driverName: name, version: version, isController: isController}
}

func (ids *IdentityServer) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	Log.Info("GetPluginInfo", "name", ids.driverName, "version", ids.version)
	return &csi.GetPluginInfoResponse{
		Name:          ids.driverName,
		VendorVersion: ids.version,
	}, nil
}

func (ids *IdentityServer) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	Log.Info("GetPluginCapabilities")
	caps := []*csi.PluginCapability{}
	if ids.isController {
		caps = append(caps, &csi.PluginCapability{
			Type: &csi.PluginCapability_Service_{
				Service: &csi.PluginCapability_Service{
					Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
				},
			},
		})
		caps = append(caps, &csi.PluginCapability{
			Type: &
		})
	}
	return &csi.GetPluginCapabilitiesResponse{Capabilities: caps}, nil
}

func (ids *IdentityServer) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	Log.Info("Probe")
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
