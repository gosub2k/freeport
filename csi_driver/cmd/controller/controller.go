package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"freeport/pkg/controller"
	"freeport/pkg/util"

	"github.com/container-storage-interface/spec/lib/go/csi"
	// "github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"google.golang.org/grpc"
)

var (
	endpoint = flag.String("endpoint", "/socketdir/csi.sock", "CSI endpoint")
	nodeID   = flag.String("nodeid", "", "Node ID")
	name     = flag.String("name", "freeport.local", "Driver name")
	version  = flag.String("version", "0.1.0", "Driver version")
)

func main() {
	// Cleanup socket if exists
	if err := os.Remove(*endpoint); err != nil && !os.IsNotExist(err) {
		log.Fatalf("Failed to remove socket: %v", err)
	}

	log.Printf("trying to listen on %v", *endpoint)

	listener, err := net.Listen("unix", *endpoint)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}
	defer listener.Close()

	// Set permissions so Kubelet/Sidecars can read
	if err := os.Chmod(*endpoint, 0777); err != nil {
		log.Fatalf("Failed to chmod socket: %v", err)
	}

	server := grpc.NewServer()
	csi.RegisterControllerServer(server, controller.NewControllerServer(*name))
	csi.RegisterIdentityServer(server, util.NewIdentityServer(*name, *version, true))

	//	csi.RegisterIdentityServer(server, controller.)

	log.Printf("Listening on %s", *endpoint)

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		server.GracefulStop()
	}()

	if err := server.Serve(listener); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
