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

// maxMountFailures caps how many times mountAll retries a device that keeps
// failing to mount before giving up on it silently. Without this, a
// permanently broken device (wrong/corrupt filesystem, dead media, ...)
// would retry and log an identical mount failure every reconcile tick
// forever.
const maxMountFailures = 3

func getDf(mountpoint string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mountpoint, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * st.Bsize
}

// isSwapPartition reports whether devPath is formatted as Linux swap, via
// blkid's on-disk superblock signature detection — sysfs exposes no fs-type
// attribute for an unmounted partition, so this is the standard way to know
// without attempting (and predictably failing) a real mount(8) call first.
// blkid failing or reporting an empty/unknown type is not treated as swap —
// mountDevice's normal mount attempt remains the fallback in that case.
func isSwapPartition(devPath string) bool {
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
//
// TODO: auto-format unrecognized filesystems before mounting. Today this
// assumes every device already carries a filesystem mkfs would have written.
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

// mountAll attempts to mount every discovered device, keyed by DevPath (not
// Serial — a single physical USB stick can carry multiple partitions, e.g.
// one valid filesystem plus a stray/uninitialized one, and they all share
// one Serial from the common USB sysfs node; keying by Serial would let a
// sibling partition's success reset a genuinely broken partition's failure
// count on every tick, so it would never actually hit the cap) in
// m.mountFailures, to skip (without re-attempting or re-logging) any
// partition that has already failed maxMountFailures times in a row. A
// partition's failure count is cleared once it mounts successfully, or once
// it's no longer discovered at all (unplugged) — so a later reinsertion (or
// a replacement device that happens to reuse the same path) gets a fresh
// set of attempts rather than being permanently blacklisted.
func (m *Manager) mountAll(discovered []devicescan.Device) []mountedDevice {
	seen := map[string]bool{}
	var mounted []mountedDevice
	for _, d := range discovered {
		seen[d.DevPath] = true
		if m.mountFailures[d.DevPath] >= maxMountFailures {
			continue
		}
		mp := m.mountFn(m.hostRoot, d)
		if mp == "" {
			m.mountFailures[d.DevPath]++
			if m.mountFailures[d.DevPath] == maxMountFailures {
				util.Log.Error("mount failed repeatedly, giving up until device is removed and reinserted",
					"dev", d.DevPath, "failures", maxMountFailures)
			}
			continue
		}
		delete(m.mountFailures, d.DevPath)
		mounted = append(mounted, mountedDevice{Device: d, mountpoint: mp, free: getDf(mp)})
	}
	for devPath := range m.mountFailures {
		if !seen[devPath] {
			delete(m.mountFailures, devPath)
		}
	}
	return mounted
}

// cleanupMountpoint unmounts and removes the canonical mountpoint directory
// left behind by a device that has just been unplugged. It refuses to
// remove a non-empty directory: that means either the unmount above failed
// (so this is still the device's real filesystem content) or something else
// is wrong, and either way silently deleting whatever's in it is exactly
// what must not happen.
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
