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
			// deviceClassKey("Acme", "USB Drive") = "acme-usb-drive"
			name:        "class key selects single device",
			vc:          map[string]string{"freeport.io/acme-usb-drive": "true"},
			wantSerials: []string{"SN123"},
		},
		{
			// deviceClassKey("Generic", "Flash Drive") = "generic-flash-drive"
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
				"freeport.io/acme-usb-drive":     "true",
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

func TestDeviceClassKey(t *testing.T) {
	tests := []struct {
		name         string
		manufacturer string
		model        string
		want         string
	}{
		{"basic concatenation", "Acme", "USB Drive", "acme-usb-drive"},
		{"raw sanitized before concat", "My Co!", "Drive 2.0", "my-co-drive-2.0"},
		{"missing manufacturer falls back to unknown", "", "USB Drive", "unknown-usb-drive"},
		{"missing model falls back to unknown", "Acme", "", "acme-unknown"},
		{"both missing fall back to unknown", "", "", "unknown-unknown"},
		{"manufacturer that sanitizes to empty falls back", "---", "USB Drive", "unknown-usb-drive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deviceClassKey(tt.manufacturer, tt.model)
			if got != tt.want {
				t.Errorf("deviceClassKey(%q, %q) = %q, want %q", tt.manufacturer, tt.model, got, tt.want)
			}
			if len(got) > deviceClassNameLimit {
				t.Errorf("result length %d exceeds %d", len(got), deviceClassNameLimit)
			}
		})
	}

	t.Run("combined length within budget is not truncated", func(t *testing.T) {
		// 31 + 1 (sep) + 31 = 63 — must survive unchanged.
		m := strings.Repeat("a", 31)
		d := strings.Repeat("b", 31)
		got := deviceClassKey(m, d)
		want := m + "-" + d
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("over-budget names are split evenly", func(t *testing.T) {
		m := strings.Repeat("a", 50)
		d := strings.Repeat("b", 50)
		got := deviceClassKey(m, d)
		if len(got) > deviceClassNameLimit {
			t.Errorf("result length %d exceeds %d: %q", len(got), deviceClassNameLimit, got)
		}
		parts := strings.SplitN(got, "-", 2)
		if len(parts) != 2 {
			t.Fatalf("expected exactly one separator, got %q", got)
		}
		if diff := len(parts[0]) - len(parts[1]); diff < -1 || diff > 1 {
			t.Errorf("halves differ by more than 1 char: %d vs %d", len(parts[0]), len(parts[1]))
		}
	})

	t.Run("short manufacturer leaves model most of the budget", func(t *testing.T) {
		m := "a"
		d := strings.Repeat("b", 70)
		got := deviceClassKey(m, d)
		want := m + "-" + strings.Repeat("b", deviceClassNameLimit-len(m)-1)
		if got != want {
			t.Errorf("got %q, want %q", got, want)
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
