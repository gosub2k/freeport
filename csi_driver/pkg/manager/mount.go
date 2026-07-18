package manager

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"freeport/pkg/devicescan"
	"freeport/pkg/util"
)

// maxMountFailures caps how many times mountAll retries.
const maxMountFailures = 3

func getDf(mountpoint string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mountpoint, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * st.Bsize
}

// isSwapPartition reports whether devPath is formatted as Linux swap.
func isSwapPartition(devPath string) bool {
	// Sysfs doesn't report fs type if its not mounted, use command.
	out, err := exec.Command("blkid", "-o", "value", "-s", "TYPE", devPath).Output()
	if err != nil {
		return false
	}
	return isSwapType(string(out))
}

func isSwapType(blkidOutput string) bool {
	return strings.TrimSpace(blkidOutput) == "swap"
}

// mountDevice ensures dev is mounted at its canonical host mountpoint.
// Returns the host-absolute mountpoint, or "" on failure.
func mountDevice(hostRoot string, dev devicescan.Device) string {
	if mp, ok := devicescan.MountedAt(hostRoot, dev.DevPath); ok {
		return mp
	}
	if isSwapPartition(dev.DevPath) {
		util.Log.Debug("skipping swap partition", "dev", dev.DevPath)
		return ""
	}

	mountpoint := devicescan.CanonicalMountpoint(dev.Serial)
	hostMountpoint := filepath.Join(hostRoot, mountpoint)
	if err := os.MkdirAll(hostMountpoint, 0750); err != nil {
		util.Log.Error("mkdir failed", "path", hostMountpoint, "err", err)
		return ""
	}

	// REVISIT: consider mounting with -o sync to reduce chance of dirty filesystem if media removed.
	if out, err := exec.Command("mount" /* "-o", "sync", */, dev.DevPath, hostMountpoint).CombinedOutput(); err != nil {
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

// cleanupMountpoint unmounts and removes the canonical mountpoint directory
// left behind by a device that has just been unplugged.
func cleanupMountpoint(hostRoot, mountpoint string) {
	hostMountpoint := filepath.Join(hostRoot, mountpoint)

	if err := syscall.Unmount(hostMountpoint, 0); err != nil && err != syscall.EINVAL && err != syscall.ENOENT {
		util.Log.Error("unmount failed for removed device", "path", hostMountpoint, "err", err)
	}

	switch err := os.Remove(hostMountpoint); {
	case err == nil:
		util.Log.Info("removed mountpoint for unplugged device", "path", hostMountpoint)
	case os.IsNotExist(err):
		// already gone — nothing to do.
	case errors.Is(err, syscall.ENOTEMPTY):
		util.Log.Error("mountpoint not empty after device removal, something went wrong — leaving it for manual inspection", "path", hostMountpoint)
	default:
		util.Log.Error("cannot remove mountpoint for removed device", "path", hostMountpoint, "err", err)
	}
}
