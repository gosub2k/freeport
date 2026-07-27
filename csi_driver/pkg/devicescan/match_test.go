package devicescan

import "testing"

func TestMatchVolumeContext(t *testing.T) {
	// Two devices: one Acme USB Drive (SN123), one Generic Flash Drive (SN456).
	devices := []Device{
		{Serial: "SN123", Model: "USB Drive", Manufacturer: "Acme", Partition: 1},
		{Serial: "SN456", Model: "Flash Drive", Manufacturer: "Generic", Partition: 1},
	}

	tests := []struct {
		name        string
		vc          map[string]string
		wantSerials []string // nil means no match
	}{
		{
			name:        "empty vc matches all devices",
			vc:          map[string]string{},
			wantSerials: []string{"SN123", "SN456"},
		},
		{
			// DeviceLabel("Acme", "USB Drive") = "acme-usb-drive"
			name:        "class key selects single device",
			vc:          map[string]string{"freeport.io/acme-usb-drive": "true"},
			wantSerials: []string{"SN123"},
		},
		{
			// DeviceLabel("Generic", "Flash Drive") = "generic-flash-drive"
			name:        "different class key selects the other device",
			vc:          map[string]string{"freeport.io/generic-flash-drive": "true"},
			wantSerials: []string{"SN456"},
		},
		{
			name:        "unknown class key matches no devices",
			vc:          map[string]string{"freeport.io/nonexistent-vendor": "true"},
			wantSerials: nil,
		},
		{
			name:        "value other than true matches no devices",
			vc:          map[string]string{"freeport.io/acme-usb-drive": "false"},
			wantSerials: nil,
		},
		{
			// A device can only ever have one class key, so two driver-prefixed
			// keys can never both match a single device.
			name: "two distinct driver keys match nothing (AND semantics)",
			vc: map[string]string{
				"freeport.io/acme-usb-drive":      "true",
				"freeport.io/generic-flash-drive": "true",
			},
			wantSerials: nil,
		},
		{
			// Keys without the driver prefix must be silently ignored
			name:        "non-driver prefix keys are ignored",
			vc:          map[string]string{"kubernetes.io/hostname": "node1"},
			wantSerials: []string{"SN123", "SN456"},
		},
		{
			// A different CSI driver's key should not affect matching
			name:        "wrong driver prefix is ignored — all devices match",
			vc:          map[string]string{"othercsi.io/acme-usb-drive": "true"},
			wantSerials: []string{"SN123", "SN456"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchVolumeContext(devices, "freeport.io", tt.vc)
			if len(got) != len(tt.wantSerials) {
				t.Fatalf("got %d devices %v, want %d %v",
					len(got), serialsOf(got), len(tt.wantSerials), tt.wantSerials)
			}
			for i, d := range got {
				if d.Serial != tt.wantSerials[i] {
					t.Errorf("got[%d].serial = %q, want %q", i, d.Serial, tt.wantSerials[i])
				}
			}
		})
	}
}

// serialsOf extracts serial fields for readable failure messages.
func serialsOf(devs []Device) []string {
	out := make([]string, len(devs))
	for i, d := range devs {
		out[i] = d.Serial
	}
	return out
}
