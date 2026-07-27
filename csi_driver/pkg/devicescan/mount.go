package devicescan

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"freeport/pkg/util"
)

// maxStaleUnmounts bounds how many stacked mounts ClearStaleMount will drain
// from one mountpoint in a single pass, so a pathological mount table cannot
// stall a reconcile tick.
const maxStaleUnmounts = 32

// Mountpoint is where d belongs, as the host sees it.
func (d Device) Mountpoint() string {
	return fmt.Sprintf("/mnt/k8s-freeport-%s", serial)
}

// HostMountpoint is Mountpoint as this process sees it.
func (d Device) HostMountpoint() string {
	return filepath.Join(d.HostRoot, d.Mountpoint())
}

// bare strips the hostRoot prefix from p. Applied to both sides of every
// comparison, so it doesn't matter which form either started in: paths this
// process resolves are hostRoot-prefixed ("/host/dev/sda1"), while
// /proc/1/mounts belongs to the real host's init and records bare ones
// ("/dev/sda1"), and the two never compare equal untrimmed.
func (d Device) bare(p string) string {
	return strings.TrimPrefix(p, d.HostRoot)
}

// MountedAt reports which source device is mounted at d's canonical
// mountpoint, if anything is.
//
// It is keyed on the mountpoint rather than on the device node because a
// device node is not stable across a replug while the serial — and therefore
// the mountpoint — is. Callers compare the returned source against the device
// they expect, so a mountpoint still held by a previous, now-unplugged node is
// recognized as stale rather than mistaken for the device being mounted.
//
// When entries stack on one mountpoint the last wins, which is the one the
// kernel resolves accesses to.
func (d Device) MountedAt() (source string, mounted bool) {
	f, err := os.Open(filepath.Join(d.HostRoot, "/proc/1/mounts"))
	if err != nil {
		util.Log.Debug("cannot read mount table", "err", err)
		return "", false
	}
	defer f.Close()

	want := d.bare(d.Mountpoint())

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) >= 2 && d.bare(parts[1]) == want {
			source, mounted = d.bare(parts[0]), true
		}
	}
	return source, mounted
}

// IsMounted reports whether d itself is currently mounted at its canonical
// mountpoint. A mountpoint left occupied by a different (usually just
// unplugged) device node is deliberately not "mounted": see MountedAt.
func (d Device) IsMounted() bool {
	source, mounted := d.MountedAt()
	return mounted && source == d.bare(d.DevPath)
}

// FreeBytes reports space available on d's mountpoint, or 0 if it can't be read.
func (d Device) FreeBytes() int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(d.HostMountpoint(), &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * st.Bsize
}

// IsSwap reports whether d is formatted as Linux swap, in which case it is not
// ours to mount.
func (d Device) IsSwap() bool {
	// Sysfs doesn't report fs type if its not mounted, use command.
	out, err := exec.Command("blkid", "-o", "value", "-s", "TYPE", d.DevPath).Output()
	if err != nil {
		return false
	}
	return isSwapType(string(out))
}

func isSwapType(blkidOutput string) bool {
	return strings.TrimSpace(blkidOutput) == "swap"
}

// Mount mounts d at its canonical mountpoint and returns that mountpoint, or
// "" on failure.
func (d Device) Mount() string {
	if d.IsSwap() {
		util.Log.Debug("skipping swap partition", "dev", d.DevPath)
		return ""
	}

	if err := os.MkdirAll(d.HostMountpoint(), 0750); err != nil {
		util.Log.Error("mkdir failed", "path", d.HostMountpoint(), "err", err)
		return ""
	}

	// REVISIT: consider mounting with -o sync to reduce chance of dirty filesystem if media removed.
	if out, err := exec.Command("mount" /* "-o", "sync", */, d.DevPath, d.HostMountpoint()).CombinedOutput(); err != nil {
		util.Log.Error("mount failed", "dev", d.DevPath, "mp", d.HostMountpoint(), "err", err, "output", strings.TrimSpace(string(out)))
		return ""
	}
	util.Log.Info("MOUNTED", "dev", d.DevPath, "mp", d.Mountpoint())
	return d.Mountpoint()
}

// ClearStaleMount unmounts anything still occupying d's canonical mountpoint
// that isn't d itself, and is a no-op when the mountpoint is free.
//
// This is the unplug/replug case. Yanking a stick doesn't unmount it, so its
// entry lingers in the mount table pointing at a device node that no longer
// exists. If it is replugged inside one reconcile interval the manager never
// observes the gap, so Unmount never runs — and because the kernel usually
// brings the stick back under a new node, the returning device no longer
// matches the entry holding its mountpoint. Mounting on top would only stack
// the live device behind a dead one, so the dead one is cleared first.
func (d Device) ClearStaleMount() {
	source, mounted := d.MountedAt()
	if !mounted {
		return
	}

	util.Log.Info("clearing stale mount before remounting reconnected device",
		"mp", d.Mountpoint(), "staleSource", source, "dev", d.DevPath)

	// Detach rather than unmount: the backing device is typically gone, and a
	// plain unmount of a dead device can block or fail EBUSY. Loops because
	// duplicates may have stacked on this mountpoint.
	for range maxStaleUnmounts {
		if err := syscall.Unmount(d.HostMountpoint(), syscall.MNT_DETACH); err != nil {
			// EINVAL means nothing is mounted here any more — fully drained.
			if !errors.Is(err, syscall.EINVAL) {
				util.Log.Error("unmounting stale mountpoint failed", "path", d.HostMountpoint(), "err", err)
			}
			return
		}
	}
	util.Log.Error("stale mountpoint still occupied after draining, leaving for manual inspection",
		"path", d.HostMountpoint(), "unmounts", maxStaleUnmounts)
}

// Unmount unmounts d and removes its mountpoint directory, for a device that
// has just been unplugged.
func (d Device) Unmount() {
	hostMountpoint := d.HostMountpoint()

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
