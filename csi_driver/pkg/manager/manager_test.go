package manager

import (
	"context"
	"reflect"
	"sort"
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"freeport/pkg/devicescan"
)

func serials(devices []devicescan.Device) []string {
	if len(devices) == 0 {
		return nil
	}
	out := make([]string, 0, len(devices))
	for _, d := range devices {
		out = append(out, d.Serial)
	}
	sort.Strings(out)
	return out
}

func TestDeviceDelta(t *testing.T) {
	a := devicescan.Device{Serial: "A"}
	b := devicescan.Device{Serial: "B"}
	c := devicescan.Device{Serial: "C"}

	tests := []struct {
		name        string
		prev, curr  map[string]devicescan.Device
		wantAdded   []string
		wantRemoved []string
	}{
		{
			name: "no change reports nothing",
			prev: map[string]devicescan.Device{"A": a, "B": b},
			curr: map[string]devicescan.Device{"A": a, "B": b},
		},
		{
			name:      "new device is added",
			prev:      map[string]devicescan.Device{"A": a},
			curr:      map[string]devicescan.Device{"A": a, "B": b},
			wantAdded: []string{"B"},
		},
		{
			name:        "unplugged device is removed",
			prev:        map[string]devicescan.Device{"A": a, "B": b},
			curr:        map[string]devicescan.Device{"A": a},
			wantRemoved: []string{"B"},
		},
		{
			name:        "add and remove in the same tick",
			prev:        map[string]devicescan.Device{"A": a, "B": b},
			curr:        map[string]devicescan.Device{"A": a, "C": c},
			wantAdded:   []string{"C"},
			wantRemoved: []string{"B"},
		},
		{
			name:      "first tick with no prior state reports everything as added",
			prev:      map[string]devicescan.Device{},
			curr:      map[string]devicescan.Device{"A": a, "B": b},
			wantAdded: []string{"A", "B"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			added, removed := deviceDelta(tt.prev, tt.curr)
			if got := serials(added); !reflect.DeepEqual(got, tt.wantAdded) {
				t.Errorf("added = %v, want %v", got, tt.wantAdded)
			}
			if got := serials(removed); !reflect.DeepEqual(got, tt.wantRemoved) {
				t.Errorf("removed = %v, want %v", got, tt.wantRemoved)
			}
		})
	}
}

func TestDesiredLabels(t *testing.T) {
	devices := []devicescan.Device{
		{Manufacturer: "Acme", Model: "USB Drive", Serial: "SN123"},
		{Manufacturer: "Generic", Model: "Flash Drive", Serial: "SN456"},
	}

	got := desiredLabels("freeport.local", devices)
	want := map[string]string{
		"freeport.local/acme-usb-drive":      "true",
		"freeport.local/generic-flash-drive": "true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("desiredLabels = %v, want %v", got, want)
	}
}

func TestDesiredLabels_noDevices(t *testing.T) {
	got := desiredLabels("freeport.local", nil)
	if len(got) != 0 {
		t.Errorf("desiredLabels(nil) = %v, want empty", got)
	}
}

func TestDiffLabels(t *testing.T) {
	tests := []struct {
		name    string
		current map[string]string
		desired map[string]string
		want    map[string]any
	}{
		{
			name:    "new label is added",
			current: map[string]string{},
			desired: map[string]string{"freeport.local/acme-usb-drive": "true"},
			want:    map[string]any{"freeport.local/acme-usb-drive": "true"},
		},
		{
			name:    "unchanged label produces no patch",
			current: map[string]string{"freeport.local/acme-usb-drive": "true"},
			desired: map[string]string{"freeport.local/acme-usb-drive": "true"},
			want:    map[string]any{},
		},
		{
			name:    "stale driver-prefixed key is deleted",
			current: map[string]string{"freeport.local/old-class": "true"},
			desired: map[string]string{},
			want:    map[string]any{"freeport.local/old-class": nil},
		},
		{
			name: "non-driver labels are never touched",
			current: map[string]string{
				"kubernetes.io/hostname": "node1",
				"freeport.local/old":     "true",
			},
			desired: map[string]string{"freeport.local/new": "true"},
			want: map[string]any{
				"freeport.local/old": nil,
				"freeport.local/new": "true",
			},
		},
		{
			name:    "add and remove in the same pass",
			current: map[string]string{"freeport.local/a": "true", "freeport.local/b": "true"},
			desired: map[string]string{"freeport.local/b": "true", "freeport.local/c": "true"},
			want: map[string]any{
				"freeport.local/a": nil,
				"freeport.local/c": "true",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nodeLabelPatch(tt.current, tt.desired, "freeport.local/")
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("diffLabels = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSyncTopologyKeys_noCSINode(t *testing.T) {
	m := &Manager{clientset: fake.NewSimpleClientset(), nodeName: "node-a", driverName: "freeport.local"}

	if err := m.syncTopologyKeys(context.Background(), nil); err != nil {
		t.Fatalf("syncTopologyKeys = %v, want nil (registrar hasn't created CSINode yet)", err)
	}
}

func TestSyncTopologyKeys_driverNotRegistered(t *testing.T) {
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Spec:       storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{Name: "other.driver", NodeID: "node-a"}}},
	}
	clientset := fake.NewSimpleClientset(csiNode)
	m := &Manager{clientset: clientset, nodeName: "node-a", driverName: "freeport.local"}

	if err := m.syncTopologyKeys(context.Background(), nil); err != nil {
		t.Fatalf("syncTopologyKeys = %v, want nil", err)
	}

	got, _ := clientset.StorageV1().CSINodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if len(got.Spec.Drivers) != 1 || got.Spec.Drivers[0].Name != "other.driver" {
		t.Errorf("Drivers = %v, want the unrelated entry left untouched", got.Spec.Drivers)
	}
}

// TestSyncTopologyKeys_updatesStaleKeys is a regression test for a real bug
// found running against a live cluster: the API server rejects a plain
// Update to CSINodeDriver.TopologyKeys with "field is immutable" — the fake
// clientset used here doesn't enforce that validation, so this test alone
// can't catch a regression back to Update; it exists to lock in that the
// delete+recreate round trip preserves everything it must (OwnerReferences
// in particular — CSINode's link back to its Node, easy to drop by accident
// when rebuilding the object from scratch).
func TestSyncTopologyKeys_updatesStaleKeys(t *testing.T) {
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-a",
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "v1", Kind: "Node", Name: "node-a", UID: "node-a-uid"},
			},
		},
		Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{
			{Name: "other.driver", NodeID: "node-a", TopologyKeys: []string{"other.driver/should-not-be-touched"}},
			{Name: "freeport.local", NodeID: "node-a", TopologyKeys: []string{"freeport.local/stale-class"}},
		}},
	}
	clientset := fake.NewSimpleClientset(csiNode)
	m := &Manager{clientset: clientset, nodeName: "node-a", driverName: "freeport.local"}

	mounted := []devicescan.Device{{Manufacturer: "SanDisk", Model: "Cruzer", Serial: "SN1"}}
	if err := m.syncTopologyKeys(context.Background(), mounted); err != nil {
		t.Fatalf("syncTopologyKeys = %v", err)
	}

	got, _ := clientset.StorageV1().CSINodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	idx := driverIndex(got.Spec.Drivers, "freeport.local")
	if idx == -1 {
		t.Fatalf("freeport.local entry disappeared: %v", got.Spec.Drivers)
	}
	want := []string{"freeport.local/sandisk-cruzer"}
	if !reflect.DeepEqual(got.Spec.Drivers[idx].TopologyKeys, want) {
		t.Errorf("TopologyKeys = %v, want %v", got.Spec.Drivers[idx].TopologyKeys, want)
	}

	otherIdx := driverIndex(got.Spec.Drivers, "other.driver")
	if otherIdx == -1 || !reflect.DeepEqual(got.Spec.Drivers[otherIdx].TopologyKeys, []string{"other.driver/should-not-be-touched"}) {
		t.Errorf("other driver's entry was touched: %v", got.Spec.Drivers)
	}

	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].UID != "node-a-uid" {
		t.Errorf("OwnerReferences = %v, want the original Node owner reference preserved across the delete+recreate", got.OwnerReferences)
	}
}

func TestSyncTopologyKeys_noopWhenAlreadyCurrent(t *testing.T) {
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a", ResourceVersion: "1"},
		Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{
			{Name: "freeport.local", NodeID: "node-a", TopologyKeys: []string{"freeport.local/sandisk-cruzer"}},
		}},
	}
	clientset := fake.NewSimpleClientset(csiNode)
	m := &Manager{clientset: clientset, nodeName: "node-a", driverName: "freeport.local"}

	mounted := []devicescan.Device{{Manufacturer: "SanDisk", Model: "Cruzer", Serial: "SN1"}}
	if err := m.syncTopologyKeys(context.Background(), mounted); err != nil {
		t.Fatalf("syncTopologyKeys = %v", err)
	}

	got, _ := clientset.StorageV1().CSINodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if got.ResourceVersion != "1" {
		t.Errorf("ResourceVersion changed to %q, want unchanged \"1\" — an Update call happened when it shouldn't have", got.ResourceVersion)
	}
}
