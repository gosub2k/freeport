package manager

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"freeport/pkg/devicescan"
	"freeport/pkg/util"
)

func getDf(mountpoint string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mountpoint, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * st.Bsize
}

// mountDevice ensures dev is mounted at its canonical host mountpoint.
// Returns the host-absolute mountpoint, or "" on failure.
//
// TODO: auto-format unrecognized filesystems before mounting. Today this
// assumes every device already carries a filesystem mkfs would have written.
func mountDevice(hostRoot string, dev devicescan.Device) string {
	if mp, ok := devicescan.MountedAt(hostRoot, dev.DevPath); ok {
		return mp
	}

	mountpoint := devicescan.CanonicalMountpoint(dev.Serial)
	hostMountpoint := filepath.Join(hostRoot, mountpoint)
	if err := os.MkdirAll(hostMountpoint, 0750); err != nil {
		util.Log.Error("mkdir failed", "path", hostMountpoint, "err", err)
		return ""
	}
	if out, err := exec.Command("mount", dev.DevPath, hostMountpoint).CombinedOutput(); err != nil {
		util.Log.Error("mount failed", "dev", dev.DevPath, "mp", hostMountpoint, "err", err, "output", strings.TrimSpace(string(out)))
		return ""
	}
	util.Log.Info("mounted", "dev", dev.DevPath, "mp", mountpoint)
	return mountpoint
}

// mountedDevice pairs a discovered device with its mountpoint and free space,
// once mounting has succeeded.
type mountedDevice struct {
	devicescan.Device
	mountpoint string
	free       int64
}

func (d mountedDevice) String() string {
	return fmt.Sprintf("%s, mountpoint: %s, free: %d", d.Device.String(), d.mountpoint, d.free)
}
