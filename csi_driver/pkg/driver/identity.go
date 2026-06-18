package driver

import (
	"context"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"freeport/pkg/util"
)

type IdentityServer struct {
	csi.UnimplementedIdentityServer
	driverName string
	version    string
}

func NewIdentityServer(name, version string) *IdentityServer {
	util.Log.Info("NewIdentityServer", "name", name, "version", version)
	return &IdentityServer{driverName: name, version: version}
}

func (ids *IdentityServer) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	util.Log.Info("GetPluginInfo", "name", ids.driverName, "version", ids.version)
	return &csi.GetPluginInfoResponse{
		Name:          ids.driverName,
		VendorVersion: ids.version,
	}, nil
}

func (ids *IdentityServer) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	util.Log.Info("GetPluginCapabilities")
	return &csi.GetPluginCapabilitiesResponse{}, nil
}

func (ids *IdentityServer) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	util.Log.Info("Probe")
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
