package manager

import (
	"reflect"
	"testing"

	storagev1 "k8s.io/api/storage/v1"

	"freeport/pkg/devicescan"
)

func TestDesiredTopologyKeys(t *testing.T) {
	devices := []mountedDevice{
		{Device: devicescan.Device{Manufacturer: "SanDisk", Model: "Cruzer", Serial: "SN1"}},
		{Device: devicescan.Device{Manufacturer: "Acme", Model: "USB Drive", Serial: "SN2"}},
	}

	got := desiredTopologyKeys("freeport.local", devices)
	want := []string{"freeport.local/acme-usb-drive", "freeport.local/sandisk-cruzer"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("desiredTopologyKeys = %v, want %v", got, want)
	}
}

func TestDesiredTopologyKeys_noDevices(t *testing.T) {
	if got := desiredTopologyKeys("freeport.local", nil); len(got) != 0 {
		t.Errorf("desiredTopologyKeys(nil) = %v, want empty", got)
	}
}

func TestDesiredTopologyKeys_alwaysSorted(t *testing.T) {
	devices := []mountedDevice{
		{Device: devicescan.Device{Manufacturer: "Zebra", Model: "Drive", Serial: "SN1"}},
		{Device: devicescan.Device{Manufacturer: "Acme", Model: "Drive", Serial: "SN2"}},
		{Device: devicescan.Device{Manufacturer: "Mid", Model: "Drive", Serial: "SN3"}},
	}
	got := desiredTopologyKeys("freeport.local", devices)
	want := []string{"freeport.local/acme-drive", "freeport.local/mid-drive", "freeport.local/zebra-drive"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("desiredTopologyKeys = %v, want sorted %v", got, want)
	}
}

func TestDriverIndex(t *testing.T) {
	drivers := []storagev1.CSINodeDriver{
		{Name: "other.driver", NodeID: "n1"},
		{Name: "freeport.local", NodeID: "n1"},
	}

	if got := driverIndex(drivers, "freeport.local"); got != 1 {
		t.Errorf("driverIndex = %d, want 1", got)
	}
	if got := driverIndex(drivers, "not.registered"); got != -1 {
		t.Errorf("driverIndex = %d, want -1 for an unregistered driver", got)
	}
	if got := driverIndex(nil, "freeport.local"); got != -1 {
		t.Errorf("driverIndex(nil) = %d, want -1", got)
	}
}

func TestTopologyKeysEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want bool
	}{
		{"both empty", nil, nil, true},
		{"same order", []string{"a", "b"}, []string{"a", "b"}, true},
		{"different order still equal", []string{"a", "b"}, []string{"b", "a"}, true},
		{"different length", []string{"a"}, []string{"a", "b"}, false},
		{"same length, different content", []string{"a", "b"}, []string{"a", "c"}, false},
		{"one empty one not", nil, []string{"a"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := topologyKeysEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("topologyKeysEqual(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			// Must be symmetric.
			if got := topologyKeysEqual(tt.b, tt.a); got != tt.want {
				t.Errorf("topologyKeysEqual(%v, %v) = %v, want %v (reversed args)", tt.b, tt.a, got, tt.want)
			}
		})
	}
}

// TestTopologyKeysEqual_doesNotMutateInputs guards against a real footgun:
// sorting the caller's slices in place would corrupt csiNode.Spec.Drivers[i].TopologyKeys
// out from under syncTopologyKeys's own equality check.
func TestTopologyKeysEqual_doesNotMutateInputs(t *testing.T) {
	a := []string{"b", "a"}
	b := []string{"a", "b"}
	aCopy := append([]string(nil), a...)
	bCopy := append([]string(nil), b...)

	topologyKeysEqual(a, b)

	if !reflect.DeepEqual(a, aCopy) {
		t.Errorf("a was mutated: got %v, want %v", a, aCopy)
	}
	if !reflect.DeepEqual(b, bCopy) {
		t.Errorf("b was mutated: got %v, want %v", b, bCopy)
	}
}
