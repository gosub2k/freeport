package tests

import (
	"flag"
	"freeport/pkg/controller"
	"freeport/pkg/driver"
	"freeport/pkg/util"
	"net"
	"os"

	//	"github.com/neilotoole/slogt"

	// "os/signal"
	// "syscall"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"google.golang.org/grpc"

	"testing"
)

var (
	endpoint       = flag.String("endpoint", "unix:///tmp/csi-test.sock", "CSI endpoint")
	nodeID         = flag.String("nodeid", "foobartest", "Node ID")
	name           = flag.String("name", "freeport.local", "Driver name")
	version        = flag.String("version", "0.1.0", "Driver version")
	verboseLogging = flag.Bool("verbose-logs", false, "Enable verbose driver logs")
)

func TestMyDriver(t *testing.T) {
	// 1. Define the endpoint where your test instance of the driver will run
	// This must match the socket your driver binds to in 'main.go' during the test

	// if *verboseLogging {
	// 	// Inject verbose logger
	// 	logger := slogt.New(t) // Debug level
	// 	util.UpdateLog(logger)
	// 	t.Log("Verbose logging enabled via -verbose-logs flag")
	// } else {
	// 	// Inject standard logger
	// 	// logger := slogt.New(t)
	// 	// util.SetLUpdateLog(logger)
	// }

	// 2. Setup the driver instance to listen on this endpoint
	// You usually start your driver in a goroutine here
	go func() {
		flag.Parse()
		sock := (*endpoint)[7:] // strip "unix://"
		os.Remove(sock)

		lis, err := net.Listen("unix", sock)
		if err != nil {
			util.Log.Error("failed to listen", "socket", sock, "err", err)
			os.Exit(1)
		}

		util.Log.Info("starting", "driver", *name, "version", *version, "node", *nodeID, "socket", sock)

		server := grpc.NewServer()
		csi.RegisterIdentityServer(server, util.NewIdentityServer(*name, *version, false))
		csi.RegisterNodeServer(server, driver.NewNodeServer(*nodeID, ""))
		csi.RegisterControllerServer(server, &controller.ControllerServer{}) // Needs controller server or even -ginko.focus="NodeGetInfo" fail

		server.Serve(lis)

	}()

	// 3. Configure the sanity test
	config := sanity.NewTestConfig()
	config.Address = *endpoint
	config.IdempotentCount = -1 // control verbosity

	// Optional: Set paths for mount targets if required by your test environment
	// config.TargetPath = "/tmp/csi/target"
	// config.StagingPath = "/tmp/csi/staging"

	// 4. Run the test suite
	sanity.Test(t, config)
}
