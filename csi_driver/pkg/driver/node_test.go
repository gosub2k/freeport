package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// NodeGetInfo deliberately never reports topology (see the comment on
// NodeGetInfo itself) — pkg/manager owns keeping CSINode's topologyKeys
// current instead, since it runs continuously rather than once at driver
// startup. This must hold regardless of what's actually plugged in.
func TestNodeGetInfo_neverReportsTopology(t *testing.T) {
	ns := NewNodeServer("test-node", "", "freeport.local", WithFakeDevice(t.TempDir()))

	resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo failed: %v", err)
	}

	if resp.NodeId != "test-node" {
		t.Errorf("NodeId = %q, want %q", resp.NodeId, "test-node")
	}
	if resp.AccessibleTopology != nil {
		t.Errorf("AccessibleTopology = %v, want nil even with a device present", resp.AccessibleTopology)
	}
}
