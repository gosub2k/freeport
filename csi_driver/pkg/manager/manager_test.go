package manager

import (
	"reflect"
	"sort"
	"testing"

	"freeport/pkg/devicescan"
)

func serials(devices []mountedDevice) []string {
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
	a := mountedDevice{Device: devicescan.Device{Serial: "A"}}
	b := mountedDevice{Device: devicescan.Device{Serial: "B"}}
	c := mountedDevice{Device: devicescan.Device{Serial: "C"}}

	tests := []struct {
		name        string
		prev, curr  map[string]mountedDevice
		wantAdded   []string
		wantRemoved []string
	}{
		{
			name: "no change reports nothing",
			prev: map[string]mountedDevice{"A": a, "B": b},
			curr: map[string]mountedDevice{"A": a, "B": b},
		},
		{
			name:      "new device is added",
			prev:      map[string]mountedDevice{"A": a},
			curr:      map[string]mountedDevice{"A": a, "B": b},
			wantAdded: []string{"B"},
		},
		{
			name:        "unplugged device is removed",
			prev:        map[string]mountedDevice{"A": a, "B": b},
			curr:        map[string]mountedDevice{"A": a},
			wantRemoved: []string{"B"},
		},
		{
			name:        "add and remove in the same tick",
			prev:        map[string]mountedDevice{"A": a, "B": b},
			curr:        map[string]mountedDevice{"A": a, "C": c},
			wantAdded:   []string{"C"},
			wantRemoved: []string{"B"},
		},
		{
			name:      "first tick with no prior state reports everything as added",
			prev:      map[string]mountedDevice{},
			curr:      map[string]mountedDevice{"A": a, "B": b},
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
	devices := []mountedDevice{
		{Device: devicescan.Device{Manufacturer: "Acme", Model: "USB Drive", Serial: "SN123"}, mountpoint: "/mnt/k8s-freeport-SN123"},
		{Device: devicescan.Device{Manufacturer: "Generic", Model: "Flash Drive", Serial: "SN456"}, mountpoint: "/mnt/k8s-freeport-SN456"},
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
