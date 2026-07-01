package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		out  string
	}{
		{"empty", "", ""},
		{"already clean", "hello", "hello"},
		{"uppercase to lower", "Hello", "hello"},
		{"space becomes dash", "Hello World", "hello-world"},
		{"underscore preserved", "USB_Drive", "usb_drive"},
		{"dot preserved", "drive2.0", "drive2.0"},
		{"special chars become dashes", "a@b#c$d", "a-b-c-d"},
		{"leading separator stripped", "---leading", "leading"},
		{"trailing separator stripped", "trailing---", "trailing"},
		{"leading and trailing dots stripped", "...dots...", "dots"},
		{"mixed separators stripped at edges", "___dot.sep___", "dot.sep"},
		{"spaces at edges stripped via dash trim", "  spaces  ", "spaces"},
		{"alphanumeric preserved", "ABC123", "abc123"},
		{"exclamation becomes dash then trimmed", "drive!", "drive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitize(tt.in)
			if got != tt.out {
				t.Errorf("sanitize(%q) = %q, want %q", tt.in, got, tt.out)
			}
		})
	}
}

func TestMatchVolumeContext(t *testing.T) {
	// Two devices: one Acme USB Drive (SN123), one Generic Flash Drive (SN456).
	devices := []hostBlockDevice{
		{serial: "SN123", model: "USB Drive", manufacturer: "Acme", partition: 1},
		{serial: "SN456", model: "Flash Drive", manufacturer: "Generic", partition: 1},
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
			name:        "type=usb matches all devices",
			vc:          map[string]string{"freeport.io/type": "usb"},
			wantSerials: []string{"SN123", "SN456"},
		},
		{
			name:        "type=nfs matches no devices",
			vc:          map[string]string{"freeport.io/type": "nfs"},
			wantSerials: nil,
		},
		{
			// sanitize("SN123") = "sn123"
			name:        "serial key selects single device",
			vc:          map[string]string{"freeport.io/serial-sn123": "true"},
			wantSerials: []string{"SN123"},
		},
		{
			// sanitize("USB Drive") = "usb-drive"
			name:        "model key selects single device",
			vc:          map[string]string{"freeport.io/model-usb-drive": "true"},
			wantSerials: []string{"SN123"},
		},
		{
			// sanitize("Acme") = "acme"
			name:        "manufacturer key selects single device",
			vc:          map[string]string{"freeport.io/manufacturer-acme": "true"},
			wantSerials: []string{"SN123"},
		},
		{
			// manufacturer matches SN123, but serial-sn456 does not — AND logic → no match
			name: "all constraints must hold (AND semantics)",
			vc: map[string]string{
				"freeport.io/manufacturer-acme": "true",
				"freeport.io/serial-sn456":      "true",
			},
			wantSerials: nil,
		},
		{
			// type=usb + serial narrows from all to one
			name: "type and serial together narrow to one device",
			vc: map[string]string{
				"freeport.io/type":          "usb",
				"freeport.io/serial-sn456":  "true",
			},
			wantSerials: []string{"SN456"},
		},
		{
			// Keys without the driver prefix must be silently ignored
			name:        "non-driver prefix keys are ignored",
			vc:          map[string]string{"kubernetes.io/hostname": "node1", "freeport.io/type": "usb"},
			wantSerials: []string{"SN123", "SN456"},
		},
		{
			// A different CSI driver's key should not affect matching
			name:        "wrong driver prefix is ignored — all devices match",
			vc:          map[string]string{"othercsi.io/serial-sn123": "true"},
			wantSerials: []string{"SN123", "SN456"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchVolumeContext(devices, "freeport.io", tt.vc)
			if len(got) != len(tt.wantSerials) {
				t.Fatalf("got %d devices %v, want %d %v",
					len(got), serialsOf(got), len(tt.wantSerials), tt.wantSerials)
			}
			for i, d := range got {
				if d.serial != tt.wantSerials[i] {
					t.Errorf("got[%d].serial = %q, want %q", i, d.serial, tt.wantSerials[i])
				}
			}
		})
	}
}

func TestUsbNodeFor(t *testing.T) {
	t.Run("finds serial file walking up hierarchy", func(t *testing.T) {
		// Simulate:
		//   <tmp>/sys/class/block/sdb1  →  <tmp>/sys/devices/usb1/port/block/sdb1  (symlink)
		//   <tmp>/sys/devices/usb1/serial                                           (file)
		tmp := t.TempDir()

		realDev := filepath.Join(tmp, "sys/devices/usb1/port/block/sdb1")
		if err := os.MkdirAll(realDev, 0755); err != nil {
			t.Fatal(err)
		}

		usbNode := filepath.Join(tmp, "sys/devices/usb1")
		if err := os.WriteFile(filepath.Join(usbNode, "serial"), []byte("SN123\n"), 0644); err != nil {
			t.Fatal(err)
		}

		sysClassBlock := filepath.Join(tmp, "sys/class/block")
		if err := os.MkdirAll(sysClassBlock, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realDev, filepath.Join(sysClassBlock, "sdb1")); err != nil {
			t.Fatal(err)
		}

		got := usbNodeFor("/dev/sdb1", sysClassBlock)
		if got != usbNode {
			t.Errorf("usbNodeFor = %q, want %q", got, usbNode)
		}
	})

	t.Run("returns empty when no serial file anywhere in hierarchy", func(t *testing.T) {
		tmp := t.TempDir()

		realDev := filepath.Join(tmp, "sys/devices/usb1/port/block/sdb1")
		if err := os.MkdirAll(realDev, 0755); err != nil {
			t.Fatal(err)
		}
		// No serial file placed anywhere.

		sysClassBlock := filepath.Join(tmp, "sys/class/block")
		if err := os.MkdirAll(sysClassBlock, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realDev, filepath.Join(sysClassBlock, "sdb1")); err != nil {
			t.Fatal(err)
		}

		got := usbNodeFor("/dev/sdb1", sysClassBlock)
		if got != "" {
			t.Errorf("usbNodeFor = %q, want empty string", got)
		}
	})

	t.Run("returns empty when device symlink is missing", func(t *testing.T) {
		tmp := t.TempDir()
		sysClassBlock := filepath.Join(tmp, "sys/class/block")
		if err := os.MkdirAll(sysClassBlock, 0755); err != nil {
			t.Fatal(err)
		}
		// No entry for sdb1 in sysClassBlock.

		got := usbNodeFor("/dev/sdb1", sysClassBlock)
		if got != "" {
			t.Errorf("usbNodeFor = %q, want empty string", got)
		}
	})

	t.Run("serial file at immediate resolved path", func(t *testing.T) {
		// serial is directly in the resolved directory (zero hops up)
		tmp := t.TempDir()

		realDev := filepath.Join(tmp, "sys/devices/usb1")
		if err := os.MkdirAll(realDev, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(realDev, "serial"), []byte("SN999\n"), 0644); err != nil {
			t.Fatal(err)
		}

		sysClassBlock := filepath.Join(tmp, "sys/class/block")
		if err := os.MkdirAll(sysClassBlock, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realDev, filepath.Join(sysClassBlock, "sdb1")); err != nil {
			t.Fatal(err)
		}

		got := usbNodeFor("/dev/sdb1", sysClassBlock)
		if got != realDev {
			t.Errorf("usbNodeFor = %q, want %q", got, realDev)
		}
	})
}

func TestLabelValue(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		raw    string
		want   string
	}{
		{"empty raw returns empty", "serial-", "", ""},
		{"raw that sanitizes to empty returns empty", "serial-", "---", ""},
		{"basic concatenation", "model-", "Samsung", "model-samsung"},
		{"raw sanitized before concat", "model-", "My Drive!", "model-my-drive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := labelValue(tt.prefix, tt.raw)
			if got != tt.want {
				t.Errorf("labelValue(%q, %q) = %q, want %q", tt.prefix, tt.raw, got, tt.want)
			}
			if len(got) > 63 {
				t.Errorf("result length %d exceeds 63", len(got))
			}
		})
	}

	t.Run("exactly 63 chars is not truncated", func(t *testing.T) {
		// "serial-" (7) + 56 'a's = 63 — must survive unchanged.
		prefix := "serial-"
		raw := strings.Repeat("a", 56)
		got := labelValue(prefix, raw)
		if len(got) != 63 {
			t.Errorf("expected length 63, got %d: %q", len(got), got)
		}
		if got != prefix+raw {
			t.Errorf("got %q, want %q", got, prefix+raw)
		}
	})

	t.Run("over 63 chars is truncated to 63", func(t *testing.T) {
		// "serial-" (7) + 57 'a's = 64 — must be capped at 63.
		prefix := "serial-"
		raw := strings.Repeat("a", 57)
		got := labelValue(prefix, raw)
		if len(got) != 63 {
			t.Errorf("expected length 63, got %d: %q", len(got), got)
		}
	})
}

// serialsOf extracts serial fields for readable failure messages.
func serialsOf(devs []hostBlockDevice) []string {
	out := make([]string, len(devs))
	for i, d := range devs {
		out[i] = d.serial
	}
	return out
}
