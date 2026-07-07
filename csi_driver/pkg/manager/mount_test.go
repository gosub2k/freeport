package manager

import (
	"os"
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
