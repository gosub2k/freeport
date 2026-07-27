package devicescan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeviceLabel(t *testing.T) {
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
			got := DeviceLabel(tt.manufacturer, tt.model)
			if got != tt.want {
				t.Errorf("DeviceClassKey(%q, %q) = %q, want %q", tt.manufacturer, tt.model, got, tt.want)
			}
			if len(got) > Maxk8sLabelLength {
				t.Errorf("result length %d exceeds %d", len(got), Maxk8sLabelLength)
			}
		})
	}

	t.Run("combined length within budget is not truncated", func(t *testing.T) {
		// 31 + 1 (sep) + 31 = 63 — must survive unchanged.
		m := strings.Repeat("a", 31)
		d := strings.Repeat("b", 31)
		got := DeviceLabel(m, d)
		want := m + "-" + d
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("over-budget names are split evenly", func(t *testing.T) {
		m := strings.Repeat("a", 50)
		d := strings.Repeat("b", 50)
		got := DeviceLabel(m, d)
		if len(got) > Maxk8sLabelLength {
			t.Errorf("result length %d exceeds %d: %q", len(got), Maxk8sLabelLength, got)
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
		got := DeviceLabel(m, d)
		want := m + "-" + strings.Repeat("b", Maxk8sLabelLength-len(m)-1)
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

// writeMounts builds a hostRoot whose /proc/1/mounts holds the given lines.
func writeMounts(t *testing.T, lines string) string {
	t.Helper()
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "proc/1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "proc/1/mounts"), []byte(lines), 0644); err != nil {
		t.Fatal(err)
	}
	return tmp
}

func TestMountedAt(t *testing.T) {
	t.Run("reports the source mounted at the device's canonical mountpoint", func(t *testing.T) {
		tmp := writeMounts(t, "/dev/sda1 / ext4 rw 0 0\n/dev/sdb1 /mnt/k8s-freeport-SN123 vfat rw 0 0\n")

		source, mounted := MountedAt(tmp, Device{Serial: "SN123", DevPath: "/dev/sdb1"})
		if !mounted || source != "/dev/sdb1" {
			t.Errorf("MountedAt = (%q, %v), want (%q, true)", source, mounted, "/dev/sdb1")
		}
	})

	t.Run("reports not-mounted when the canonical mountpoint is free", func(t *testing.T) {
		tmp := writeMounts(t, "/dev/sda1 / ext4 rw 0 0\n")

		if _, mounted := MountedAt(tmp, Device{Serial: "SN123", DevPath: "/dev/sdb1"}); mounted {
			t.Error("MountedAt = mounted, want not-mounted")
		}
	})

	t.Run("reports not-mounted when the mounts file is missing", func(t *testing.T) {
		if _, mounted := MountedAt(t.TempDir(), Device{Serial: "SN123", DevPath: "/dev/sdb1"}); mounted {
			t.Error("MountedAt = mounted, want not-mounted")
		}
	})

	t.Run("last entry wins when mounts are stacked on one mountpoint", func(t *testing.T) {
		tmp := writeMounts(t, "/dev/sdb1 /mnt/k8s-freeport-SN123 vfat rw 0 0\n/dev/sdc1 /mnt/k8s-freeport-SN123 vfat rw 0 0\n")

		source, mounted := MountedAt(tmp, Device{Serial: "SN123", DevPath: "/dev/sdc1"})
		if !mounted || source != "/dev/sdc1" {
			t.Errorf("MountedAt = (%q, %v), want (%q, true) — the kernel resolves to the topmost mount", source, mounted, "/dev/sdc1")
		}
	})
}

func TestIsMounted(t *testing.T) {
	const mounts = "/dev/sdb1 /mnt/k8s-freeport-SN123 vfat rw 0 0\n"

	t.Run("true when this device holds its own canonical mountpoint", func(t *testing.T) {
		tmp := writeMounts(t, mounts)
		if !IsMounted(tmp, Device{Serial: "SN123", DevPath: "/dev/sdb1"}) {
			t.Error("IsMounted = false, want true")
		}
	})

	// Regression test for a real bug found on a live cluster: mount tables with
	// tens of thousands of stacked duplicate mounts, because the idempotency
	// check compared a hostRoot-prefixed path against a bare one and so never
	// matched anything.
	t.Run("true for a hostRoot-prefixed DevPath against the bare path PID 1 records", func(t *testing.T) {
		tmp := writeMounts(t, mounts)
		// What Discover() actually hands us: hostRoot-prefixed.
		dev := Device{Serial: "SN123", DevPath: filepath.Join(tmp, "/dev/sdb1")}
		if !IsMounted(tmp, dev) {
			t.Errorf("IsMounted(%q, %q) = false, want true", tmp, dev.DevPath)
		}
	})

	// Regression test for pods failing NodePublishVolume with "no matching
	// block devices" after a device was unplugged and replugged: the kernel
	// re-enumerates it under a new node, so the mountpoint is still held by
	// the old one. That is a stale mount to clear, not a mounted device.
	t.Run("false when the mountpoint is held by a different, unplugged device node", func(t *testing.T) {
		tmp := writeMounts(t, mounts)
		if IsMounted(tmp, Device{Serial: "SN123", DevPath: "/dev/sdc1"}) {
			t.Error("IsMounted = true for a mountpoint held by /dev/sdb1, want false")
		}
	})

	t.Run("false when nothing is mounted at the canonical mountpoint", func(t *testing.T) {
		tmp := writeMounts(t, "/dev/sda1 / ext4 rw 0 0\n")
		if IsMounted(tmp, Device{Serial: "SN123", DevPath: "/dev/sdb1"}) {
			t.Error("IsMounted = true, want false")
		}
	})
}
