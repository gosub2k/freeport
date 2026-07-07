package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestNodeGetInfo_reportsAccessibleTopology(t *testing.T) {
	ns := NewNodeServer("test-node", "", "freeport.local", WithFakeDevice(t.TempDir()))

	resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo failed: %v", err)
	}

	if resp.AccessibleTopology == nil {
		t.Fatal("AccessibleTopology is nil, want it populated from the scanned device")
	}
	// WithFakeDevice reports manufacturer "Test Co", model "Test USB" ->
	// devicescan.DeviceClassKey("Test Co", "Test USB") = "test-co-test-usb".
	want := "freeport.local/test-co-test-usb"
	got, ok := resp.AccessibleTopology.Segments[want]
	if !ok || got != "true" {
		t.Errorf("Segments = %v, want %q=true", resp.AccessibleTopology.Segments, want)
	}
}

func TestNodeGetInfo_noDevicesMeansNoTopology(t *testing.T) {
	ns := NewNodeServer("test-node", "", "freeport.local", WithNoScan())

	resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo failed: %v", err)
	}

	if resp.AccessibleTopology != nil {
		t.Errorf("AccessibleTopology = %v, want nil when no devices are present", resp.AccessibleTopology)
	}
}
