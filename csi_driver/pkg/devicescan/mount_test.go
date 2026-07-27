package devicescan

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMount_notMountedAttemptsRealMount(t *testing.T) {
	// Without root/CAP_SYS_ADMIN, the real `mount` command will fail — this
	// just confirms Mount falls through to attempting it (and fails
	// gracefully) rather than mis-reporting an unmounted device as mounted.
	tmp := writeMounts(t, "")

	dev := Device{Serial: "SN999", DevPath: "/dev/does-not-exist", HostRoot: tmp}
	if got := dev.Mount(); got != "" {
		t.Errorf("Mount = %q, want empty (mount of nonexistent device must fail)", got)
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

// mkswapFile builds a real Linux swap signature so IsSwap exercises blkid
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

func TestIsSwap_realSwapSignature(t *testing.T) {
	f := filepath.Join(t.TempDir(), "swapfile")
	mkswapFile(t, f)

	if !(Device{DevPath: f}).IsSwap() {
		t.Errorf("IsSwap() = false, want true for a real swap signature at %q", f)
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
	tmp := t.TempDir()
	dev := Device{Serial: "SN1", HostRoot: tmp}
	if err := os.MkdirAll(dev.HostMountpoint(), 0750); err != nil {
		t.Fatal(err)
	}

	dev.Unmount()

	if _, err := os.Stat(dev.HostMountpoint()); !os.IsNotExist(err) {
		t.Errorf("Unmount should have removed the empty directory, stat err = %v", err)
	}
}

func TestUnmount_refusesNonEmptyDir(t *testing.T) {
	tmp := t.TempDir()
	dev := Device{Serial: "SN1", HostRoot: tmp}
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
