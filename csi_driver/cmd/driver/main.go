package main

import (
	"flag"
	"net"
	"os"

	"freeport/pkg/driver"
	"freeport/pkg/util"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
)

var (
	endpoint = flag.String("endpoint", "unix:///var/lib/kubelet/plugins/local.freeport/csi.sock", "CSI endpoint")
	nodeID   = flag.String("nodeid", "", "Node ID")
	name     = flag.String("name", "freeport.local", "Driver name")
	version  = flag.String("version", "0.1.0", "Driver version")
	hostRoot = os.Getenv("HOST_ROOT")
)

func main() {
	flag.Parse()
	sock := (*endpoint)[7:] // strip "unix://"
	os.Remove(sock)

	lis, err := net.Listen("unix", sock)
	if err != nil {
		util.Log.Error("failed to listen", "socket", sock, "err", err)
		os.Exit(1)
	}

	util.Log.Info("starting", "driver", *name, "version", *version, "node", *nodeID, "socket", sock)

	if hostRoot == "" {
		util.Log.Info("HOST_ROOT not set")
	}

	server := grpc.NewServer()
	csi.RegisterIdentityServer(server, util.NewIdentityServer(*name, *version, false))
	csi.RegisterNodeServer(server, driver.NewNodeServer(*nodeID, hostRoot))

	server.Serve(lis)
}
