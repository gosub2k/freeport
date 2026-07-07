package tests

import (
	"flag"
	"net"
	"os"
	"strings"
	"testing"

	"freeport/pkg/controller"
	"freeport/pkg/driver"
	"freeport/pkg/util"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
)

var (
	endpoint = flag.String("endpoint", "unix:///tmp/csi-test.sock", "CSI endpoint")
	nodeID   = flag.String("nodeid", "foobartest", "Node ID")
	name     = flag.String("name", "freeport.local", "Driver name")
	version  = flag.String("version", "0.1.0", "Driver version")
)

// startDriver starts an in-process CSI driver and returns a base TestConfig
// pointed at its socket. The socket is removed before Listen and on t.Cleanup.
func startDriver(t *testing.T, nodeOpts ...func(*driver.NodeServer)) sanity.TestConfig {
	t.Helper()
	sock := strings.TrimPrefix(*endpoint, "unix://")
	os.Remove(sock)
	t.Cleanup(func() { os.Remove(sock) })

	ready := make(chan struct{})
	errchan := make(chan error, 1)
	go func() {
		lis, err := net.Listen("unix", sock)
		if err != nil {
			errchan <- err
			return
		}
		close(ready)

		util.Log.Info("starting", "driver", *name, "version", *version, "node", *nodeID, "socket", sock)
		server := grpc.NewServer()
		csi.RegisterIdentityServer(server, util.NewIdentityServer(*name, *version, false))
		csi.RegisterNodeServer(server, driver.NewNodeServer(*nodeID, "", *name, nodeOpts...))
		csi.RegisterControllerServer(server, controller.NewControllerServer(*name))
		server.Serve(lis)
	}()

	select {
	case <-ready:
	case err := <-errchan:
		t.Fatal("driver failed to start:", err)
	}

	// Use fresh temp paths so concurrent or repeated runs don't collide with the
	// default /tmp/csi-mount (os.Mkdir fails EEXIST if the directory already exists).
	tmp := t.TempDir()
	cfg := sanity.NewTestConfig()
	cfg.Address = *endpoint
	cfg.TargetPath = tmp + "/target"
	cfg.StagingPath = tmp + "/staging"
	cfg.IdempotentCount = 0 // set > 0 to exercise CSI idempotency guarantees
	return cfg
}

// runFocused runs the csi-test Ginkgo suite restricted to specs whose full
// path matches focus as a regex. It replicates sanity.Test but overrides
// FocusStrings so each caller sees only its own spec group.
//
// Ginkgo v2 guards against RunSpecs being called twice in the same binary
// (suiteDidRun flag → os.Exit). Each Test* function below therefore MUST be
// invoked via a separate `go test -run TestX` process. TEST.sh does this;
// plain `go test ./test/` (no -run) will exit on the second function.
func runFocused(t *testing.T, focus string, nodeOpts ...func(*driver.NodeServer)) {
	cfg := startDriver(t, nodeOpts...)
	tc := sanity.GinkgoTest(&cfg)
	RegisterFailHandler(Fail)
	suiteConfig, reporterConfig := GinkgoConfiguration()
	suiteConfig.FocusStrings = []string{focus}
	RunSpecs(t, "CSI Driver Test Suite", suiteConfig, reporterConfig)
	tc.Finalize()
}

// Top-level Describe groups in csi-test v5 (used as focus patterns):
//
//	"Identity Service"
//	"Node Service"
//	"Controller Service"          – matches "Controller Service [Controller Server]"
//	"ListSnapshots [Controller Server]"
//	"DeleteSnapshot [Controller Server]"
//	"CreateSnapshot [Controller Server]"
//	"ExpandVolume [Controller Server]"

func TestIdentityService(t *testing.T) {
	runFocused(t, "Identity Service", driver.WithNoScan())
}

func TestNodeService(t *testing.T) {
	// WithFakeDevice provides one synthetic device so matchVolumeContext has a
	// candidate. WithNoMount skips the real bind mount (requires CAP_SYS_ADMIN).
	runFocused(t, "Node Service", driver.WithFakeDevice(t.TempDir()), driver.WithNoMount())
}

func TestControllerService(t *testing.T) {
	runFocused(t, "Controller Service", driver.WithNoScan())
}

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(m.Run())
}
