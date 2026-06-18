package main

import (
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"freeport/pkg/controller"

	"github.com/container-storage-interface/spec/lib/go/csi"
	// "github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"google.golang.org/grpc"
)

func main() {
	// Cleanup socket if exists
	if err := os.Remove(controller.SocketPath); err != nil && !os.IsNotExist(err) {
		log.Fatalf("Failed to remove socket: %v", err)
	}

	listener, err := net.Listen("unix", controller.SocketPath)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}
	defer listener.Close()

	// Set permissions so Kubelet/Sidecars can read
	if err := os.Chmod(controller.SocketPath, 0777); err != nil {
		log.Fatalf("Failed to chmod socket: %v", err)
	}

	server := grpc.NewServer()
	csi.RegisterControllerServer(server, &controller.ControllerServer{})

	log.Printf("Listening on %s", controller.SocketPath)

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
