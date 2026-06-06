package main

import (
    "context"
    "github.com/container-storage-interface/spec/lib/go/csi"
    "google.golang.org/protobuf/types/known/wrapperspb"
)

type IdentityServer struct {
    csi.UnimplementedIdentityServer
    driverName string
    version    string
}

func NewIdentityServer(name, version string) *IdentityServer {
    return &IdentityServer{driverName: name, version: version}
}

func (ids *IdentityServer) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
    return &csi.GetPluginInfoResponse{
        Name:          ids.driverName,
        VendorVersion: ids.version,
    }, nil
}

func (ids *IdentityServer) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
    return &csi.GetPluginCapabilitiesResponse{}, nil
}

func (ids *IdentityServer) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
    return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
