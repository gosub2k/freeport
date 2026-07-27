package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanReadyDevices(t *testing.T) {
	t.Run("only devices already mounted by the manager are returned", func(t *testing.T) {
		tmp := t.TempDir()
		devRoot := filepath.Join(tmp, "dev")
		devDir := filepath.Join(tmp, "dev/disk/by-id")
		sysClassBlock := filepath.Join(tmp, "sys/class/block")
		procDir := filepath.Join(tmp, "proc/1")
		for _, d := range []string{devRoot, devDir, sysClassBlock, procDir} {
			if err := os.MkdirAll(d, 0755); err != nil {
				t.Fatal(err)
			}
		}

		// Device 1: ready — mounted by the manager. "sdb1" is both the real
		// device node under /dev and the sysfs entry under
		// /sys/class/block, linking the two the way the kernel does.
		realDev1 := filepath.Join(devRoot, "sdb1")
		writeAttr(t, devRoot, "sdb1", "")
		readyDev := filepath.Join(tmp, "sys/devices/usb1")
		if err := os.MkdirAll(readyDev, 0755); err != nil {
			t.Fatal(err)
		}
		writeAttr(t, readyDev, "serial", "SN123")
		writeAttr(t, readyDev, "manufacturer", "Acme")
		writeAttr(t, readyDev, "product", "USB Drive")
		if err := os.Symlink(readyDev, filepath.Join(sysClassBlock, "sdb1")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realDev1, filepath.Join(devDir, "usb-ready-part1")); err != nil {
			t.Fatal(err)
		}

		// Device 2: discovered but not yet mounted by the manager.
		realDev2 := filepath.Join(devRoot, "sdc1")
		writeAttr(t, devRoot, "sdc1", "")
		notReadyDev := filepath.Join(tmp, "sys/devices/usb2")
		if err := os.MkdirAll(notReadyDev, 0755); err != nil {
			t.Fatal(err)
		}
		writeAttr(t, notReadyDev, "serial", "SN456")
		if err := os.Symlink(notReadyDev, filepath.Join(sysClassBlock, "sdc1")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realDev2, filepath.Join(devDir, "usb-notready-part1")); err != nil {
			t.Fatal(err)
		}

		// /proc/1/mounts belongs to PID 1 — the real host's own init, with no
		// notion of our hostRoot bind-mount prefix — so it always records
		// the bare host path, never the tmp(hostRoot)-prefixed one Discover()
		// resolves realDev1 to. See devicescan.MountedAt's doc comment.
		bareDev1 := strings.TrimPrefix(realDev1, tmp)
		mounts := bareDev1 + " /mnt/k8s-freeport-SN123 vfat rw 0 0\n"
		if err := os.WriteFile(filepath.Join(procDir, "mounts"), []byte(mounts), 0644); err != nil {
			t.Fatal(err)
		}

		got := scanReadyDevices(tmp)
		if len(got) != 1 {
			t.Fatalf("got %d ready devices, want 1: %+v", len(got), got)
		}
		if got[0].serial != "SN123" || got[0].mountpoint != "/mnt/k8s-freeport-SN123" {
			t.Errorf("got %+v, want serial SN123 mounted at /mnt/k8s-freeport-SN123", got[0])
		}
		if got[0].manufacturer != "Acme" || got[0].model != "USB Drive" {
			t.Errorf("got manufacturer=%q model=%q, want Acme/USB Drive", got[0].manufacturer, got[0].model)
		}
	})

	// Regression test for NodePublishVolume failing "no matching block devices
	// on node" after an unplug/replug. The device is back under a new node,
	// but its canonical mountpoint is still held by the old one, so what is
	// mounted there is dead. Reporting it ready would hand a pod a mountpoint
	// backed by a device that no longer exists; the manager clears the stale
	// entry and remounts, and only then is the device ready.
	t.Run("device whose mountpoint is still held by its previous node is not ready", func(t *testing.T) {
		tmp := t.TempDir()
		devRoot := filepath.Join(tmp, "dev")
		devDir := filepath.Join(tmp, "dev/disk/by-id")
		sysClassBlock := filepath.Join(tmp, "sys/class/block")
		procDir := filepath.Join(tmp, "proc/1")
		for _, d := range []string{devRoot, devDir, sysClassBlock, procDir} {
			if err := os.MkdirAll(d, 0755); err != nil {
				t.Fatal(err)
			}
		}

		// The stick came back as sdc1; the mount table still records sdb1.
		realDev := filepath.Join(devRoot, "sdc1")
		writeAttr(t, devRoot, "sdc1", "")
		usbNode := filepath.Join(tmp, "sys/devices/usb1")
		if err := os.MkdirAll(usbNode, 0755); err != nil {
			t.Fatal(err)
		}
		writeAttr(t, usbNode, "serial", "SN123")
		writeAttr(t, usbNode, "manufacturer", "Acme")
		writeAttr(t, usbNode, "product", "USB Drive")
		if err := os.Symlink(usbNode, filepath.Join(sysClassBlock, "sdc1")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realDev, filepath.Join(devDir, "usb-reconnected-part1")); err != nil {
			t.Fatal(err)
		}

		mounts := "/dev/sdb1 /mnt/k8s-freeport-SN123 vfat rw 0 0\n"
		if err := os.WriteFile(filepath.Join(procDir, "mounts"), []byte(mounts), 0644); err != nil {
			t.Fatal(err)
		}

		if got := scanReadyDevices(tmp); len(got) != 0 {
			t.Errorf("got %+v, want no ready devices — /mnt/k8s-freeport-SN123 is still held by the unplugged /dev/sdb1", got)
		}
	})

	t.Run("no devices discovered returns nil", func(t *testing.T) {
		tmp := t.TempDir()
		got := scanReadyDevices(tmp)
		if got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})
}

func writeAttr(t *testing.T, dir, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
}
