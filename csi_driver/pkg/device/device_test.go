package device

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Helpers
//////////

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

func writeAttr(t *testing.T, dir, name, value string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

// hostRootFixture builds the directory skeleton Discover walks, with the given
// /proc/1/mounts contents.
func hostRootFixture(t *testing.T, mounts string) string {
	t.Helper()
	tmp := writeMounts(t, mounts)
	for _, d := range []string{"dev/disk/by-id", "sys/class/block"} {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return tmp
}

// addUSBPartition wires one USB partition into a hostRootFixture the way the
// kernel does, and returns the resolved device node:
//
//	<root>/dev/disk/by-id/usb-<byID>-part1 -> <root>/dev/<node>       (what Discover globs)
//	<root>/sys/class/block/<node>          -> <root>/sys/devices/<blockDir>
//
// attrs are written to <root>/sys/devices/<attrDir>, which may be an ancestor
// of blockDir — that is how real sysfs looks, and it is what makes Discover
// walk up from the block device to the USB node carrying "serial". Passing nil
// attrs leaves no "serial" anywhere, so the device is unrecognizable.
func addUSBPartition(t *testing.T, root, byID, node, blockDir, attrDir string, attrs map[string]string) string {
	t.Helper()

	devNode := filepath.Join(root, "dev", node)
	writeAttr(t, filepath.Join(root, "dev"), node, "") // the device node itself

	block := filepath.Join(root, "sys/devices", blockDir)
	if err := os.MkdirAll(block, 0755); err != nil {
		t.Fatal(err)
	}
	for name, value := range attrs {
		writeAttr(t, filepath.Join(root, "sys/devices", attrDir), name, value)
	}
	if err := os.Symlink(block, filepath.Join(root, "sys/class/block", node)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(devNode, filepath.Join(root, "dev/disk/by-id", "usb-"+byID+"-part1")); err != nil {
		t.Fatal(err)
	}
	return devNode
}

// serialsOf extracts serials for readable failure messages.
func serialsOf(devs []Device) []string {
	out := make([]string, len(devs))
	for i, d := range devs {
		out[i] = d.Serial
	}
	return out
}

// mkswapFile writes a real Linux swap signature so IsSwap exercises blkid
// rather than a stub.
func mkswapFile(t *testing.T, path string) {
	t.Helper()
	if _, err := exec.LookPath("mkswap"); err != nil {
		t.Skip("mkswap not available")
	}
	if err := os.WriteFile(path, make([]byte, 2*1024*1024), 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mkswap", path).CombinedOutput(); err != nil {
		t.Skipf("mkswap failed, skipping: %v: %s", err, out)
	}
}

// Naming
/////////

func TestLabel(t *testing.T) {
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
			got := Device{Manufacturer: tt.manufacturer, Model: tt.model}.Label()
			if got != tt.want {
				t.Errorf("Label() for manufacturer %q model %q = %q, want %q", tt.manufacturer, tt.model, got, tt.want)
			}
			if len(got) > Maxk8sLabelLength {
				t.Errorf("result length %d exceeds %d", len(got), Maxk8sLabelLength)
			}
		})
	}

	// Manufacturer leads, model follows — swapping the two would still produce
	// a plausible-looking label, so it needs asserting explicitly.
	t.Run("manufacturer comes before model", func(t *testing.T) {
		got := Device{Manufacturer: "aaa", Model: "zzz"}.Label()
		if got != "aaa-zzz" {
			t.Errorf("Label() = %q, want %q — manufacturer and model are reversed", got, "aaa-zzz")
		}
	})

	t.Run("combined length within budget is not truncated", func(t *testing.T) {
		// 31 + 1 (sep) + 31 = 63 — must survive unchanged.
		m := strings.Repeat("a", 31)
		d := strings.Repeat("b", 31)
		got := Device{Manufacturer: m, Model: d}.Label()
		want := m + "-" + d
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("over-budget names are split evenly", func(t *testing.T) {
		m := strings.Repeat("a", 50)
		d := strings.Repeat("b", 50)
		got := Device{Manufacturer: m, Model: d}.Label()
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
		got := Device{Manufacturer: m, Model: d}.Label()
		want := m + "-" + strings.Repeat("b", Maxk8sLabelLength-len(m)-1)
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// Discovery
////////////

func TestDiscover(t *testing.T) {
	// The sysfs attributes live several directories above the block device, so
	// this only passes if Discover walks up to the node carrying "serial".
	t.Run("resolves attributes from an ancestor sysfs node", func(t *testing.T) {
		root := hostRootFixture(t, "")
		addUSBPartition(t, root, "ready", "sdb1", "usb1/port/block/sdb1", "usb1", map[string]string{
			"serial": "SN123", "manufacturer": "Acme", "product": "USB Drive",
		})

		got := Discover(root)
		if len(got) != 1 {
			t.Fatalf("got %d devices, want 1: %+v", len(got), got)
		}
		if got[0].Serial != "SN123" || got[0].Manufacturer != "Acme" || got[0].Model != "USB Drive" {
			t.Errorf("got %+v, want serial SN123, Acme, USB Drive", got[0])
		}
		if got[0].Partition != 1 {
			t.Errorf("Partition = %d, want 1", got[0].Partition)
		}
		if got[0].HostRoot != root {
			t.Errorf("HostRoot = %q, want %q — mount methods need it", got[0].HostRoot, root)
		}
	})

	t.Run("attributes directly on the resolved block device are found too", func(t *testing.T) {
		root := hostRootFixture(t, "")
		addUSBPartition(t, root, "ready", "sdb1", "usb1", "usb1", map[string]string{"serial": "SN999"})

		got := Discover(root)
		if len(got) != 1 || got[0].Serial != "SN999" {
			t.Fatalf("got %+v, want one device with serial SN999", got)
		}
	})

	t.Run("device with no serial anywhere in the hierarchy is skipped", func(t *testing.T) {
		root := hostRootFixture(t, "")
		addUSBPartition(t, root, "noserial", "sdb1", "usb1/port/block/sdb1", "usb1", nil)

		if got := Discover(root); len(got) != 0 {
			t.Errorf("got %+v, want none — a device with no serial cannot be identified", got)
		}
	})

	t.Run("device with no sysfs block entry is skipped", func(t *testing.T) {
		root := hostRootFixture(t, "")
		// by-id symlink and device node, but nothing under /sys/class/block.
		devNode := filepath.Join(root, "dev", "sdb1")
		writeAttr(t, filepath.Join(root, "dev"), "sdb1", "")
		if err := os.Symlink(devNode, filepath.Join(root, "dev/disk/by-id", "usb-orphan-part1")); err != nil {
			t.Fatal(err)
		}

		if got := Discover(root); len(got) != 0 {
			t.Errorf("got %+v, want none", got)
		}
	})

	t.Run("missing by-id directory returns nil rather than failing", func(t *testing.T) {
		if got := Discover(t.TempDir()); got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})
}

func TestDiscoverMounted(t *testing.T) {
	t.Run("returns only devices already mounted at their canonical mountpoint", func(t *testing.T) {
		root := hostRootFixture(t, "")
		// Device 1: mounted by the manager.
		mountedNode := addUSBPartition(t, root, "ready", "sdb1", "usb1", "usb1", map[string]string{
			"serial": "SN123", "manufacturer": "Acme", "product": "USB Drive",
		})
		// Device 2: discovered but not yet mounted.
		addUSBPartition(t, root, "notready", "sdc1", "usb2", "usb2", map[string]string{"serial": "SN456"})

		// /proc/1/mounts belongs to PID 1 — the real host's own init, with no
		// notion of our hostRoot bind-mount prefix — so it records the bare
		// host path, never the root-prefixed one Discover resolves to.
		writeAttr(t, filepath.Join(root, "proc/1"), "mounts",
			strings.TrimPrefix(mountedNode, root)+" /mnt/k8s-freeport-SN123 vfat rw 0 0")

		got := DiscoverMounted(root)
		if len(got) != 1 {
			t.Fatalf("got %d ready devices %v, want 1 (SN123)", len(got), serialsOf(got))
		}
		if got[0].Serial != "SN123" || got[0].Mountpoint() != "/mnt/k8s-freeport-SN123" {
			t.Errorf("got %+v, want serial SN123 mounted at /mnt/k8s-freeport-SN123", got[0])
		}
	})

	// Regression test for NodePublishVolume failing "no matching block devices
	// on node" after an unplug/replug. The device is back under a new node, but
	// its canonical mountpoint is still held by the old one, so what is mounted
	// there is dead. Reporting it ready would hand a pod a mountpoint backed by
	// a device that no longer exists; the manager clears the stale entry and
	// remounts, and only then is the device ready.
	t.Run("device whose mountpoint is still held by its previous node is not ready", func(t *testing.T) {
		// Came back as sdc1; the mount table still records sdb1.
		root := hostRootFixture(t, "/dev/sdb1 /mnt/k8s-freeport-SN123 vfat rw 0 0\n")
		addUSBPartition(t, root, "reconnected", "sdc1", "usb1", "usb1", map[string]string{
			"serial": "SN123", "manufacturer": "Acme", "product": "USB Drive",
		})

		if got := DiscoverMounted(root); len(got) != 0 {
			t.Errorf("got %+v, want none — /mnt/k8s-freeport-SN123 is still held by the unplugged /dev/sdb1", got)
		}
	})

	t.Run("no devices discovered returns nil", func(t *testing.T) {
		if got := DiscoverMounted(t.TempDir()); got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})
}

// Mount state
//////////////

func TestMountedAt(t *testing.T) {
	t.Run("reports the source mounted at the device's canonical mountpoint", func(t *testing.T) {
		tmp := writeMounts(t, "/dev/sda1 / ext4 rw 0 0\n/dev/sdb1 /mnt/k8s-freeport-SN123 vfat rw 0 0\n")

		source, mounted := Device{Serial: "SN123", DevPath: "/dev/sdb1", HostRoot: tmp}.MountedAt()
		if !mounted || source != "/dev/sdb1" {
			t.Errorf("MountedAt = (%q, %v), want (%q, true)", source, mounted, "/dev/sdb1")
		}
	})

	t.Run("reports not-mounted when the canonical mountpoint is free", func(t *testing.T) {
		tmp := writeMounts(t, "/dev/sda1 / ext4 rw 0 0\n")

		if _, mounted := (Device{Serial: "SN123", DevPath: "/dev/sdb1", HostRoot: tmp}).MountedAt(); mounted {
			t.Error("MountedAt = mounted, want not-mounted")
		}
	})

	t.Run("reports not-mounted when the mounts file is missing", func(t *testing.T) {
		if _, mounted := (Device{Serial: "SN123", DevPath: "/dev/sdb1", HostRoot: t.TempDir()}).MountedAt(); mounted {
			t.Error("MountedAt = mounted, want not-mounted")
		}
	})

	t.Run("last entry wins when mounts are stacked on one mountpoint", func(t *testing.T) {
		tmp := writeMounts(t, "/dev/sdb1 /mnt/k8s-freeport-SN123 vfat rw 0 0\n/dev/sdc1 /mnt/k8s-freeport-SN123 vfat rw 0 0\n")

		source, mounted := Device{Serial: "SN123", DevPath: "/dev/sdc1", HostRoot: tmp}.MountedAt()
		if !mounted || source != "/dev/sdc1" {
			t.Errorf("MountedAt = (%q, %v), want (%q, true) — the kernel resolves to the topmost mount", source, mounted, "/dev/sdc1")
		}
	})
}

func TestIsMounted(t *testing.T) {
	const mounts = "/dev/sdb1 /mnt/k8s-freeport-SN123 vfat rw 0 0\n"

	t.Run("true when this device holds its own canonical mountpoint", func(t *testing.T) {
		tmp := writeMounts(t, mounts)
		if !(Device{Serial: "SN123", DevPath: "/dev/sdb1", HostRoot: tmp}).IsMounted() {
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
		dev := Device{Serial: "SN123", DevPath: filepath.Join(tmp, "/dev/sdb1"), HostRoot: tmp}
		if !dev.IsMounted() {
			t.Errorf("IsMounted() = false for DevPath %q under hostRoot %q, want true", dev.DevPath, tmp)
		}
	})

	// Regression test for pods failing NodePublishVolume with "no matching
	// block devices" after a device was unplugged and replugged: the kernel
	// re-enumerates it under a new node, so the mountpoint is still held by
	// the old one. That is a stale mount to clear, not a mounted device.
	t.Run("false when the mountpoint is held by a different, unplugged device node", func(t *testing.T) {
		tmp := writeMounts(t, mounts)
		if (Device{Serial: "SN123", DevPath: "/dev/sdc1", HostRoot: tmp}).IsMounted() {
			t.Error("IsMounted = true for a mountpoint held by /dev/sdb1, want false")
		}
	})

	t.Run("false when nothing is mounted at the canonical mountpoint", func(t *testing.T) {
		tmp := writeMounts(t, "/dev/sda1 / ext4 rw 0 0\n")
		if (Device{Serial: "SN123", DevPath: "/dev/sdb1", HostRoot: tmp}).IsMounted() {
			t.Error("IsMounted = true, want false")
		}
	})
}

// Mounting
///////////

func TestMount_notMountedAttemptsRealMount(t *testing.T) {
	// Without root/CAP_SYS_ADMIN the real `mount` command will fail — this just
	// confirms Mount falls through to attempting it (and fails gracefully)
	// rather than mis-reporting an unmounted device as mounted.
	tmp := writeMounts(t, "")

	dev := Device{Serial: "SN999", DevPath: "/dev/does-not-exist", HostRoot: tmp}
	if got := dev.Mount(); got != "" {
		t.Errorf("Mount = %q, want empty (mount of nonexistent device must fail)", got)
	}
}

func TestIsSwap_realSwapSignature(t *testing.T) {
	f := filepath.Join(t.TempDir(), "swapfile")
	mkswapFile(t, f)

	if !(Device{DevPath: f}).IsSwap() {
		t.Errorf("IsSwap() = false, want true for a real swap signature at %q", f)
	}
}

func TestIsSwap_nonSwapDevice(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notswap")
	if err := os.WriteFile(f, make([]byte, 1024), 0644); err != nil {
		t.Fatal(err)
	}
	if (Device{DevPath: f}).IsSwap() {
		t.Error("IsSwap() = true for a file with no swap signature, want false")
	}
}

func TestMount_skipsSwapWithoutAttemptingMount(t *testing.T) {
	tmp := writeMounts(t, "")
	swapPath := filepath.Join(tmp, "swapdev")
	mkswapFile(t, swapPath)

	dev := Device{Serial: "SNSWAP", DevPath: swapPath, HostRoot: tmp}
	if got := dev.Mount(); got != "" {
		t.Errorf("Mount = %q, want empty for a swap partition", got)
	}
	if _, err := os.Stat(dev.HostMountpoint()); !os.IsNotExist(err) {
		t.Error("Mount should not have created a mountpoint directory for a swap partition")
	}
}

func TestUnmount_removesEmptyDir(t *testing.T) {
	dev := Device{Serial: "SN1", HostRoot: t.TempDir()}
	if err := os.MkdirAll(dev.HostMountpoint(), 0750); err != nil {
		t.Fatal(err)
	}

	dev.Unmount()

	if _, err := os.Stat(dev.HostMountpoint()); !os.IsNotExist(err) {
		t.Errorf("Unmount should have removed the empty directory, stat err = %v", err)
	}
}

func TestUnmount_refusesNonEmptyDir(t *testing.T) {
	dev := Device{Serial: "SN1", HostRoot: t.TempDir()}
	if err := os.MkdirAll(dev.HostMountpoint(), 0750); err != nil {
		t.Fatal(err)
	}
	leftoverFile := filepath.Join(dev.HostMountpoint(), "vol-freeport-still-here")
	if err := os.WriteFile(leftoverFile, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	dev.Unmount()

	if _, err := os.Stat(leftoverFile); err != nil {
		t.Errorf("Unmount must not touch a non-empty mountpoint's contents, but stat failed: %v", err)
	}
}

// Mounter
///////////

// newTestMounter returns a Mounter whose mountFn is replaced by fn, so no real
// mount(8) is attempted.
func newTestMounter(fn func(Device) string) *Mounter {
	mo := NewMounter()
	mo.mountFn = fn
	return mo
}

// Regression test for pods failing NodePublishVolume with "no matching block
// devices on node" after a device was unplugged and replugged.
//
// Yanking a stick doesn't unmount it, so its entry lingers in the mount table.
// Replug it inside one reconcile interval and the manager never observes the
// gap, so Unmount never runs — and the kernel usually brings the stick back
// under a *new* device node. Treating the still-occupied mountpoint as proof
// the device is mounted made the manager skip it while labelling the node, so
// pods scheduled there and then could not publish.
func TestEnsureMounted_remountsDeviceThatReturnedOnANewDevPath(t *testing.T) {
	tmp := writeMounts(t, "/dev/sdb1 /mnt/k8s-freeport-SN123 vfat rw 0 0\n")

	var mountedDev string
	mo := newTestMounter(func(d Device) string {
		mountedDev = d.DevPath
		return d.Mountpoint()
	})

	// Same serial, new device node — the mountpoint is still held by /dev/sdb1.
	got := mo.EnsureMounted([]Device{{Serial: "SN123", DevPath: "/dev/sdc1", HostRoot: tmp}})

	if mountedDev != "/dev/sdc1" {
		t.Errorf("mountFn called with %q, want /dev/sdc1 — the reconnected device must be remounted, not skipped", mountedDev)
	}
	if len(got) != 1 || got[0].Mountpoint() != "/mnt/k8s-freeport-SN123" {
		t.Fatalf("EnsureMounted = %+v, want the device reported at its canonical mountpoint", got)
	}
}

// The other half of the same decision: a device genuinely mounted where it
// belongs must not be mounted again, which is what stacked duplicate mounts.
func TestEnsureMounted_skipsDeviceAlreadyMountedAtItsCanonicalMountpoint(t *testing.T) {
	tmp := writeMounts(t, "/dev/sdb1 /mnt/k8s-freeport-SN123 vfat rw 0 0\n")

	calls := 0
	mo := newTestMounter(func(Device) string { calls++; return "" })

	// hostRoot-prefixed, as Discover() actually yields it.
	dev := Device{Serial: "SN123", DevPath: filepath.Join(tmp, "/dev/sdb1"), HostRoot: tmp}
	got := mo.EnsureMounted([]Device{dev})

	if calls != 0 {
		t.Errorf("mountFn called %d times, want 0 — remounting stacks a duplicate", calls)
	}
	if len(got) != 1 || got[0].Mountpoint() != "/mnt/k8s-freeport-SN123" {
		t.Fatalf("EnsureMounted = %+v, want the already-mounted device reported once", got)
	}
}

func TestEnsureMounted_givesUpAfterMaxFailures(t *testing.T) {
	calls := 0
	mo := newTestMounter(func(Device) string { calls++; return "" })
	dev := Device{Serial: "SN1", DevPath: "/dev/sdx1"}

	for i := 0; i < 5; i++ {
		got := mo.EnsureMounted([]Device{dev})
		if len(got) != 0 {
			t.Fatalf("pass %d: EnsureMounted = %v, want empty", i, got)
		}
	}

	if calls != maxMountFailures {
		t.Errorf("mountFn called %d times, want exactly %d (further passes should skip)", calls, maxMountFailures)
	}
	if mo.failures["/dev/sdx1"] != maxMountFailures {
		t.Errorf("failures[/dev/sdx1] = %d, want %d", mo.failures["/dev/sdx1"], maxMountFailures)
	}
}

func TestEnsureMounted_successResetsFailureCount(t *testing.T) {
	attempt := 0
	mo := newTestMounter(func(d Device) string {
		attempt++
		if attempt <= 2 {
			return ""
		}
		return d.Mountpoint()
	})
	dev := Device{Serial: "SN1", DevPath: "/dev/sdx1"}

	mo.EnsureMounted([]Device{dev})
	mo.EnsureMounted([]Device{dev})
	if mo.failures["/dev/sdx1"] != 2 {
		t.Fatalf("failures[/dev/sdx1] = %d, want 2 before the successful attempt", mo.failures["/dev/sdx1"])
	}

	got := mo.EnsureMounted([]Device{dev})
	if len(got) != 1 || got[0].Mountpoint() != "/mnt/k8s-freeport-SN1" {
		t.Fatalf("EnsureMounted = %v, want the device mounted on the 3rd attempt", got)
	}
	if _, stillTracked := mo.failures["/dev/sdx1"]; stillTracked {
		t.Errorf("failures[/dev/sdx1] should have been cleared on success, still present: %v", mo.failures)
	}
}

func TestEnsureMounted_deviceGoneResetsFailureCount(t *testing.T) {
	mo := newTestMounter(func(Device) string { return "" })
	dev := Device{Serial: "SN1", DevPath: "/dev/sdx1"}

	mo.EnsureMounted([]Device{dev})
	mo.EnsureMounted([]Device{dev})
	if mo.failures["/dev/sdx1"] != 2 {
		t.Fatalf("failures[/dev/sdx1] = %d, want 2", mo.failures["/dev/sdx1"])
	}

	mo.EnsureMounted(nil) // device unplugged — not in this pass's discovered list
	if _, tracked := mo.failures["/dev/sdx1"]; tracked {
		t.Fatalf("failures[/dev/sdx1] should be cleared once the device is no longer discovered, still present: %v", mo.failures)
	}

	mo.EnsureMounted([]Device{dev}) // reinserted — should get a fresh attempt budget
	if mo.failures["/dev/sdx1"] != 1 {
		t.Errorf("failures[/dev/sdx1] = %d after reinsertion, want 1 (fresh budget, not resumed at 2)", mo.failures["/dev/sdx1"])
	}
}

// Regression test for a real bug found running against actual hardware: a
// physical USB stick with a good partition (e.g. sdf1, mounts fine) and a
// genuinely broken sibling (e.g. sdf3, bad superblock) share one Serial from
// the common USB sysfs node. Keying failures by Serial meant sdf1's success
// cleared the shared counter every tick before sdf3 could ever accumulate 3
// consecutive failures — the cap silently never engaged, and "mount failed"
// logged forever. Keying by DevPath instead fixes this.
func TestEnsureMounted_siblingPartitionSuccessDoesNotResetFailureCount(t *testing.T) {
	goodPartition := Device{Serial: "SHARED", DevPath: "/dev/sdf1", Partition: 1}
	badPartition := Device{Serial: "SHARED", DevPath: "/dev/sdf3", Partition: 3}

	mo := newTestMounter(func(d Device) string {
		if d.DevPath == goodPartition.DevPath {
			return d.Mountpoint()
		}
		return "" // badPartition always fails, every tick, forever
	})

	for i := 0; i < 5; i++ {
		mo.EnsureMounted([]Device{goodPartition, badPartition})
	}

	if mo.failures["/dev/sdf3"] != maxMountFailures {
		t.Errorf("failures[/dev/sdf3] = %d, want %d — the good sibling partition's success must not reset the bad one's count",
			mo.failures["/dev/sdf3"], maxMountFailures)
	}
	if _, tracked := mo.failures["/dev/sdf1"]; tracked {
		t.Errorf("failures[/dev/sdf1] should never be tracked, it always succeeds: %v", mo.failures)
	}
}

// Volume context matching
//////////////////////////

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
			// Device{Manufacturer: "Acme", Model: "USB Drive"}.Label() = "acme-usb-drive"
			name:        "label selects single device",
			vc:          map[string]string{"freeport.io/acme-usb-drive": "true"},
			wantSerials: []string{"SN123"},
		},
		{
			// Device{Manufacturer: "Generic", Model: "Flash Drive"}.Label() = "generic-flash-drive"
			name:        "different label selects the other device",
			vc:          map[string]string{"freeport.io/generic-flash-drive": "true"},
			wantSerials: []string{"SN456"},
		},
		{
			name:        "unknown label matches no devices",
			vc:          map[string]string{"freeport.io/nonexistent-vendor": "true"},
			wantSerials: nil,
		},
		{
			name:        "value other than true matches no devices",
			vc:          map[string]string{"freeport.io/acme-usb-drive": "false"},
			wantSerials: nil,
		},
		{
			// A device can only ever have one label, so two driver-prefixed
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
					t.Errorf("got[%d].Serial = %q, want %q", i, d.Serial, tt.wantSerials[i])
				}
			}
		})
	}
}
