package manager

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"freeport/pkg/devicescan"
)

func TestMountDevice_alreadyMountedIsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	procDir := filepath.Join(tmp, "proc/1")
	if err := os.MkdirAll(procDir, 0755); err != nil {
		t.Fatal(err)
	}
	mounts := "/dev/sdb1 /mnt/k8s-freeport-SN123 vfat rw 0 0\n"
	if err := os.WriteFile(filepath.Join(procDir, "mounts"), []byte(mounts), 0644); err != nil {
		t.Fatal(err)
	}

	dev := devicescan.Device{Serial: "SN123", DevPath: "/dev/sdb1"}
	got := mountDevice(tmp, dev)
	if got != "/mnt/k8s-freeport-SN123" {
		t.Errorf("mountDevice = %q, want %q (no real mount(8) should have been attempted)", got, "/mnt/k8s-freeport-SN123")
	}
}

func TestMountDevice_notMountedAttemptsRealMount(t *testing.T) {
	// Without root/CAP_SYS_ADMIN, the real `mount` command will fail — this
	// just confirms mountDevice falls through to attempting it (and fails
	// gracefully) rather than mis-reporting an unmounted device as mounted.
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "proc/1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "proc/1/mounts"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	dev := devicescan.Device{Serial: "SN999", DevPath: "/dev/does-not-exist"}
	got := mountDevice(tmp, dev)
	if got != "" {
		t.Errorf("mountDevice = %q, want empty (mount of nonexistent device must fail)", got)
	}
}

func TestIsSwapType(t *testing.T) {
	tests := []struct {
		blkidOutput string
		want        bool
	}{
		{"swap\n", true},
		{"swap", true},
		{"vfat\n", false},
		{"ext4\n", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isSwapType(tt.blkidOutput); got != tt.want {
			t.Errorf("isSwapType(%q) = %v, want %v", tt.blkidOutput, got, tt.want)
		}
	}
}

func TestIsSwapPartition_realSwapSignature(t *testing.T) {
	if _, err := exec.LookPath("mkswap"); err != nil {
		t.Skip("mkswap not available")
	}
	f := filepath.Join(t.TempDir(), "swapfile")
	if err := os.WriteFile(f, make([]byte, 2*1024*1024), 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mkswap", f).CombinedOutput(); err != nil {
		t.Skipf("mkswap failed, skipping: %v: %s", err, out)
	}

	if !isSwapPartition(f) {
		t.Errorf("isSwapPartition(%q) = false, want true for a real swap signature", f)
	}
}

func TestMountDevice_skipsSwapWithoutAttemptingMount(t *testing.T) {
	if _, err := exec.LookPath("mkswap"); err != nil {
		t.Skip("mkswap not available")
	}
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "proc/1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "proc/1/mounts"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	swapPath := filepath.Join(tmp, "swapdev")
	if err := os.WriteFile(swapPath, make([]byte, 2*1024*1024), 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mkswap", swapPath).CombinedOutput(); err != nil {
		t.Skipf("mkswap failed, skipping: %v: %s", err, out)
	}

	dev := devicescan.Device{Serial: "SNSWAP", DevPath: swapPath}
	if got := mountDevice(tmp, dev); got != "" {
		t.Errorf("mountDevice = %q, want empty for a swap partition", got)
	}
	if _, err := os.Stat(filepath.Join(tmp, devicescan.CanonicalMountpoint("SNSWAP"))); !os.IsNotExist(err) {
		t.Error("mountDevice should not have created a mountpoint directory for a swap partition")
	}
}

func TestMountAll_givesUpAfterMaxFailures(t *testing.T) {
	calls := 0
	m := &Manager{
		mountFailures: map[string]int{},
		mountFn: func(hostRoot string, dev devicescan.Device) string {
			calls++
			return ""
		},
	}
	dev := devicescan.Device{Serial: "SN1", DevPath: "/dev/sdx1"}

	for i := 0; i < 5; i++ {
		got := m.mountAll([]devicescan.Device{dev})
		if len(got) != 0 {
			t.Fatalf("pass %d: mountAll = %v, want empty", i, got)
		}
	}

	if calls != maxMountFailures {
		t.Errorf("mountFn called %d times, want exactly %d (further passes should skip)", calls, maxMountFailures)
	}
	if m.mountFailures["/dev/sdx1"] != maxMountFailures {
		t.Errorf("mountFailures[/dev/sdx1] = %d, want %d", m.mountFailures["/dev/sdx1"], maxMountFailures)
	}
}

func TestMountAll_successResetsFailureCount(t *testing.T) {
	attempt := 0
	m := &Manager{
		mountFailures: map[string]int{},
		mountFn: func(hostRoot string, dev devicescan.Device) string {
			attempt++
			if attempt <= 2 {
				return ""
			}
			return "/mnt/k8s-freeport-SN1"
		},
	}
	dev := devicescan.Device{Serial: "SN1", DevPath: "/dev/sdx1"}

	m.mountAll([]devicescan.Device{dev})
	m.mountAll([]devicescan.Device{dev})
	if m.mountFailures["/dev/sdx1"] != 2 {
		t.Fatalf("mountFailures[/dev/sdx1] = %d, want 2 before the successful attempt", m.mountFailures["/dev/sdx1"])
	}

	got := m.mountAll([]devicescan.Device{dev})
	if len(got) != 1 || got[0].mountpoint != "/mnt/k8s-freeport-SN1" {
		t.Fatalf("mountAll = %v, want the device mounted on the 3rd attempt", got)
	}
	if _, stillTracked := m.mountFailures["/dev/sdx1"]; stillTracked {
		t.Errorf("mountFailures[/dev/sdx1] should have been cleared on success, still present: %v", m.mountFailures)
	}
}

func TestMountAll_deviceGoneResetsFailureCount(t *testing.T) {
	m := &Manager{
		mountFailures: map[string]int{},
		mountFn: func(hostRoot string, dev devicescan.Device) string {
			return ""
		},
	}
	dev := devicescan.Device{Serial: "SN1", DevPath: "/dev/sdx1"}

	m.mountAll([]devicescan.Device{dev})
	m.mountAll([]devicescan.Device{dev})
	if m.mountFailures["/dev/sdx1"] != 2 {
		t.Fatalf("mountFailures[/dev/sdx1] = %d, want 2", m.mountFailures["/dev/sdx1"])
	}

	m.mountAll(nil) // device unplugged — not in this pass's discovered list
	if _, tracked := m.mountFailures["/dev/sdx1"]; tracked {
		t.Fatalf("mountFailures[/dev/sdx1] should be cleared once the device is no longer discovered, still present: %v", m.mountFailures)
	}

	m.mountAll([]devicescan.Device{dev}) // reinserted — should get a fresh attempt budget
	if m.mountFailures["/dev/sdx1"] != 1 {
		t.Errorf("mountFailures[/dev/sdx1] = %d after reinsertion, want 1 (fresh budget, not resumed at 2)", m.mountFailures["/dev/sdx1"])
	}
}

// TestMountAll_siblingPartitionSuccessDoesNotResetFailureCount is a
// regression test for a real bug found running against actual hardware: a
// physical USB stick with a good partition (e.g. sdf1, mounts fine) and a
// genuinely broken sibling (e.g. sdf3, bad superblock) share one Serial from
// the common USB sysfs node. Keying mountFailures by Serial meant sdf1's
// success cleared the shared counter every tick before sdf3 could ever
// accumulate 3 consecutive failures — the cap silently never engaged, and
// "mount failed" logged forever. Keying by DevPath instead fixes this.
func TestMountAll_siblingPartitionSuccessDoesNotResetFailureCount(t *testing.T) {
	goodPartition := devicescan.Device{Serial: "SHARED", DevPath: "/dev/sdf1", Partition: 1}
	badPartition := devicescan.Device{Serial: "SHARED", DevPath: "/dev/sdf3", Partition: 3}

	m := &Manager{
		mountFailures: map[string]int{},
		mountFn: func(hostRoot string, dev devicescan.Device) string {
			if dev.DevPath == goodPartition.DevPath {
				return "/mnt/k8s-freeport-SHARED"
			}
			return "" // badPartition always fails, every tick, forever
		},
	}

	for i := 0; i < 5; i++ {
		m.mountAll([]devicescan.Device{goodPartition, badPartition})
	}

	if m.mountFailures["/dev/sdf3"] != maxMountFailures {
		t.Errorf("mountFailures[/dev/sdf3] = %d, want %d — the good sibling partition's success must not reset the bad one's count",
			m.mountFailures["/dev/sdf3"], maxMountFailures)
	}
	if _, tracked := m.mountFailures["/dev/sdf1"]; tracked {
		t.Errorf("mountFailures[/dev/sdf1] should never be tracked, it always succeeds: %v", m.mountFailures)
	}
}

func TestCleanupMountpoint_removesEmptyDir(t *testing.T) {
	tmp := t.TempDir()
	mountpoint := "/mnt/k8s-freeport-SN1"
	hostMountpoint := filepath.Join(tmp, mountpoint)
	if err := os.MkdirAll(hostMountpoint, 0750); err != nil {
		t.Fatal(err)
	}

	cleanupMountpoint(tmp, mountpoint)

	if _, err := os.Stat(hostMountpoint); !os.IsNotExist(err) {
		t.Errorf("cleanupMountpoint should have removed the empty directory, stat err = %v", err)
	}
}

func TestCleanupMountpoint_refusesNonEmptyDir(t *testing.T) {
	tmp := t.TempDir()
	mountpoint := "/mnt/k8s-freeport-SN1"
	hostMountpoint := filepath.Join(tmp, mountpoint)
	if err := os.MkdirAll(hostMountpoint, 0750); err != nil {
		t.Fatal(err)
	}
	leftoverFile := filepath.Join(hostMountpoint, "vol-freeport-still-here")
	if err := os.WriteFile(leftoverFile, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	cleanupMountpoint(tmp, mountpoint)

	if _, err := os.Stat(leftoverFile); err != nil {
		t.Errorf("cleanupMountpoint must not touch a non-empty mountpoint's contents, but stat failed: %v", err)
	}
}
