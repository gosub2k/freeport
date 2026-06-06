package main

import (
	"flag"
	"log/slog"
	"net"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
)

var (
	endpoint = flag.String("endpoint", "unix:///var/lib/kubelet/plugins/local.freeport/csi.sock", "CSI endpoint")
	nodeID   = flag.String("nodeid", "", "Node ID")
	name     = flag.String("name", "local.freeport", "Driver name")
	version  = flag.String("version", "0.1.0", "Driver version")
)

var log = slog.Default()

func main() {
	flag.Parse()
	sock := (*endpoint)[7:] // strip "unix://"
	os.Remove(sock)

	lis, err := net.Listen("unix", sock)
	if err != nil {
		log.Error("failed to listen", "socket", sock, "err", err)
		os.Exit(1)
	}

	log.Info("starting", "driver", *name, "version", *version, "node", *nodeID, "socket", sock)

	server := grpc.NewServer()
	csi.RegisterIdentityServer(server, NewIdentityServer(*name, *version))
	csi.RegisterNodeServer(server, NewNodeServer(*nodeID))

	server.Serve(lis)
}
