package devicescan

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
			got := Sanitize(tt.in)
			if got != tt.out {
				t.Errorf("Sanitize(%q) = %q, want %q", tt.in, got, tt.out)
			}
		})
	}
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
			got := DeviceClassKey(tt.manufacturer, tt.model)
			if got != tt.want {
				t.Errorf("DeviceClassKey(%q, %q) = %q, want %q", tt.manufacturer, tt.model, got, tt.want)
			}
			if len(got) > DeviceClassNameLimit {
				t.Errorf("result length %d exceeds %d", len(got), DeviceClassNameLimit)
			}
		})
	}

	t.Run("combined length within budget is not truncated", func(t *testing.T) {
		// 31 + 1 (sep) + 31 = 63 — must survive unchanged.
		m := strings.Repeat("a", 31)
		d := strings.Repeat("b", 31)
		got := DeviceClassKey(m, d)
		want := m + "-" + d
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("over-budget names are split evenly", func(t *testing.T) {
		m := strings.Repeat("a", 50)
		d := strings.Repeat("b", 50)
		got := DeviceClassKey(m, d)
		if len(got) > DeviceClassNameLimit {
			t.Errorf("result length %d exceeds %d: %q", len(got), DeviceClassNameLimit, got)
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
		got := DeviceClassKey(m, d)
		want := m + "-" + strings.Repeat("b", DeviceClassNameLimit-len(m)-1)
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
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

func TestMountedAt(t *testing.T) {
	t.Run("returns mountpoint when device is present in mounts", func(t *testing.T) {
		tmp := t.TempDir()
		procDir := filepath.Join(tmp, "proc/1")
		if err := os.MkdirAll(procDir, 0755); err != nil {
			t.Fatal(err)
		}
		mounts := "/dev/sda1 / ext4 rw 0 0\n/dev/sdb1 /mnt/k8s-freeport-SN123 vfat rw 0 0\n"
		if err := os.WriteFile(filepath.Join(procDir, "mounts"), []byte(mounts), 0644); err != nil {
			t.Fatal(err)
		}

		mp, ok := MountedAt(tmp, "/dev/sdb1")
		if !ok || mp != "/mnt/k8s-freeport-SN123" {
			t.Errorf("MountedAt = (%q, %v), want (%q, true)", mp, ok, "/mnt/k8s-freeport-SN123")
		}
	})

	t.Run("returns not-ok when device is absent", func(t *testing.T) {
		tmp := t.TempDir()
		procDir := filepath.Join(tmp, "proc/1")
		if err := os.MkdirAll(procDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(procDir, "mounts"), []byte("/dev/sda1 / ext4 rw 0 0\n"), 0644); err != nil {
			t.Fatal(err)
		}

		_, ok := MountedAt(tmp, "/dev/sdb1")
		if ok {
			t.Errorf("MountedAt = ok, want not-ok")
		}
	})

	t.Run("returns not-ok when mounts file is missing", func(t *testing.T) {
		tmp := t.TempDir()
		_, ok := MountedAt(tmp, "/dev/sdb1")
		if ok {
			t.Errorf("MountedAt = ok, want not-ok")
		}
	})

	// Regression test for a real bug found on a live cluster: mount tables
	// with tens of thousands of stacked duplicate mounts, because this
	// idempotency check never actually matched anything.
	//
	// Discover() resolves DevPath by calling filepath.EvalSymlinks on a path
	// already joined with hostRoot (e.g. "<hostRoot>/dev/disk/by-id/usb-...-part1"),
	// so the resolved DevPath comes back hostRoot-prefixed too (e.g.
	// "<hostRoot>/dev/sdb1"). But /proc/1/mounts is PID 1's own mount table —
	// PID 1 is the real host's init, not something living inside our
	// container's bind-mounted hostRoot view — so the source device it
	// records is the bare host path ("/dev/sdb1"), never hostRoot-prefixed.
	// Comparing those two directly, as the previous implementation did,
	// means the two forms can never match: mountDevice's "already mounted?"
	// check always said no, so it mounted again every single reconcile
	// tick, forever, stacking one more duplicate mount on top each time.
	t.Run("matches a hostRoot-prefixed devPath against the bare host path /proc/1/mounts actually records", func(t *testing.T) {
		tmp := t.TempDir()
		procDir := filepath.Join(tmp, "proc/1")
		if err := os.MkdirAll(procDir, 0755); err != nil {
			t.Fatal(err)
		}
		// What PID 1 (the real host) actually sees: no hostRoot prefix.
		mounts := "/dev/sdb1 /mnt/k8s-freeport-SN123 vfat rw 0 0\n"
		if err := os.WriteFile(filepath.Join(procDir, "mounts"), []byte(mounts), 0644); err != nil {
			t.Fatal(err)
		}

		// What Discover() actually hands mountDevice/MountedAt: hostRoot-prefixed.
		devPath := filepath.Join(tmp, "/dev/sdb1")

		mp, ok := MountedAt(tmp, devPath)
		if !ok || mp != "/mnt/k8s-freeport-SN123" {
			t.Errorf("MountedAt(%q, %q) = (%q, %v), want (%q, true)", tmp, devPath, mp, ok, "/mnt/k8s-freeport-SN123")
		}
	})
}
