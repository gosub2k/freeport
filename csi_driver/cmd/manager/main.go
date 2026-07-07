package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"freeport/pkg/manager"
	"freeport/pkg/util"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	nodeName          = flag.String("nodename", os.Getenv("NODE_NAME"), "Node name")
	name              = flag.String("name", "freeport.local", "Driver name")
	reconcileInterval = flag.Duration("reconcile-interval", 15*time.Second, "How often to rescan devices and reconcile node labels")
	hostRoot          = os.Getenv("HOST_ROOT")
)

func main() {
	flag.Parse()

	if *nodeName == "" {
		util.Log.Error("node name is required (--nodename or NODE_NAME)")
		os.Exit(1)
	}
	if hostRoot == "" {
		util.Log.Info("HOST_ROOT not set")
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		util.Log.Error("failed to load in-cluster config", "err", err)
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		util.Log.Error("failed to create kubernetes client", "err", err)
		os.Exit(1)
	}

	util.Log.Info("starting", "manager", *name, "node", *nodeName, "interval", *reconcileInterval)

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	mgr := manager.New(clientset, *nodeName, hostRoot, *name)
	mgr.Run(ctx, *reconcileInterval)
}
