// Package device owns USB block devices: discovering them via sysfs, the
// naming rules derived from them, mounting them, and
// answering "is this device mounted where we expect it?".
//
// It is the one place both pkg/manager (which mounts devices) and pkg/driver
// (which publishes what the manager prepared) get their answers from, so the
// two cannot drift apart.
package device

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"freeport/pkg/util"
)

const Maxk8sLabelLength = 63

// Device is a USB block device partition discovered via sysfs.
type Device struct {
	Manufacturer string
	Model        string
	Serial       string
	Partition    int
	DevPath      string // real device node, e.g. "/dev/sdb1"

	// HostRoot is where the real host's filesystem is bind-mounted.
	HostRoot string
}

func (d Device) String() string {
	partition := strconv.Itoa(d.Partition)
	if d.Partition == 0 {
		partition = "??"
	}
	return fmt.Sprintf("serial: %s, manufacturer: %q, type: usb, model: %q, partition: %s", d.Serial, d.Manufacturer, d.Model, partition)
}

// Label is the device's "<manufacturer>-<model>" topology name.
func (dv Device) Label() string {
	manufacturer, model := dv.Manufacturer, dv.Model

	// See https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#syntax-and-character-set
	k8sLabel := func(s string) string {
		s = strings.ToLower(s)
		var b strings.Builder
		for _, r := range s {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
				b.WriteRune(r)
			} else {
				b.WriteByte('-')
			}
		}
		return strings.Trim(b.String(), "-_.")
	}

	m := k8sLabel(manufacturer)
	if m == "" {
		m = "unknown"
	}
	d := k8sLabel(model)
	if d == "" {
		d = "unknown"
	}

	const sep = "-"
	budget := Maxk8sLabelLength - len(sep)
	if len(m)+len(d) > budget {
		half := budget / 2
		m = m[:min(len(m), half)]
		d = d[:min(len(d), budget-len(m))]
		m = strings.TrimRight(m, "-_.")
		d = strings.TrimRight(d, "-_.")
	}
	return m + sep + d
}

// Avoid stall on a pathological mount table.
const maxStaleUnmounts = 32

// Mountpoint is where d belongs, as the host sees it.
func (d Device) Mountpoint() string {
	return fmt.Sprintf("/mnt/k8s-freeport-%s", d.Serial)
}

// HostMountpoint is Mountpoint as this process sees it.
func (d Device) HostMountpoint() string {
	return filepath.Join(d.HostRoot, d.Mountpoint())
}

// bare strips the hostRoot prefix from p.
func (d Device) bare(p string) string {
	return strings.TrimPrefix(p, d.HostRoot)
}

// mountEntry reports the source device and mount options recorded at d's
// canonical mountpoint. When mounts are stacked the last entry wins, which is
// the one the kernel resolves accesses to.
func (d Device) mountEntry() (source, opts string, mounted bool) {
	f, err := os.Open(filepath.Join(d.HostRoot, "/proc/1/mounts"))
	if err != nil {
		util.Log.Debug("cannot read mount table", "err", err)
		return "", "", false
	}
	defer f.Close()

	want := d.bare(d.Mountpoint())

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) >= 2 && d.bare(parts[1]) == want {
			source, mounted = d.bare(parts[0]), true
			opts = ""
			if len(parts) >= 4 {
				opts = parts[3]
			}
		}
	}
	return source, opts, mounted
}

// MountInfo reports which source device is mounted at d's canonical
// mountpoint, if anything is.
func (d Device) MountInfo() (source string, mounted bool) {
	source, _, mounted = d.mountEntry()
	return source, mounted
}

// IsReadOnly reports whether d is mounted read-only. The kernel silently falls
// back to a read-only mount when it will not write to a filesystem, so a device
// can mount "successfully" and still be useless for volumes.
func (d Device) IsReadOnly() bool {
	_, opts, mounted := d.mountEntry()
	if !mounted {
		return false
	}
	for _, o := range strings.Split(opts, ",") {
		if o == "ro" {
			return true
		}
	}
	return false
}

// IsMounted reports whether d itself is currently mounted at its canonical
// mountpoint. A mountpoint left occupied by a different (usually just
// unplugged) device node is deliberately not "mounted": see MountedAt.
func (d Device) IsMounted() bool {
	source, mounted := d.MountInfo()
	// REVISIT: which one is it?
	return mounted && (source == d.bare(d.DevPath) || source == d.DevPath)
}

// FreeBytes reports space available on d's mountpoint, or 0 if it can't be read.
func (d Device) FreeBytes() int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(d.HostMountpoint(), &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * st.Bsize
}

// IsSwap reports whether d is formatted as Linux swap.
func (d Device) IsSwap() bool {
	// Sysfs doesn't report fs type if its not mounted, use command.
	out, err := exec.Command("blkid", "-o", "value", "-s", "TYPE", d.DevPath).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "swap"
}

/*
	switch fstype {
	case "ext2", "ext3", "ext4":
		// -y: assume yes to all questions, -f: force check even if clean
		return "e2fsck", []string{"-y", "-f", device}, true
	case "vfat":
		// dosfsck/fsck.vfat: -a = auto-repair non-interactively, -w = write immediately
		return "fsck.vfat", []string{"-a", "-w", device}, true
	case "ntfs", "ntfs-3g":
		// ntfsfix clears the dirty flag and fixes some common problems;
		// it's not a full fsck but it's the standard non-interactive tool
		return "ntfsfix", []string{device}, true
	case "xfs":
		// fsck.xfs is a no-op stub by design; use xfs_repair directly
		return "xfs_repair", []string{device}, true
	case "btrfs":
		// btrfs check --repair is the closest analog; still requires unmounted fs
		return "btrfs", []string{"check", "--repair", device}, true
	case "exfat":
		return "fsck.exfat", []string{"-y", device}, true
	case "jfs":
		return "fsck.jfs", []string{"-y", device}, true
	case "reiserfs":
		return "reiserfsck", []string{"-y", "--fix-fixable", device}, true
	default:
		return "", nil, false

*/

func runCmd(com ...string) (error, string, string) {
	var stdout, stderr bytes.Buffer

	cmd := exec.Command(com[0], com[1:]...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return err, strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String())
}

// MaybeRepair tries to fix a filesystem to prevent mounting read-only.
// Read-only mount could occur if a usb device was removed before syncing fs.
// Its intentionally aggressive, an option to control this could be added later.
//
// force skips the "is it already clean?" check and asks the checker to run even
// when the superblock claims the filesystem is clean — for when the fs looks
// fine but the kernel still refused to mount it writable.
func (d Device) MaybeRepair(force bool) {

	if !force {
		// Check filesystem
		util.Log.Info("CHECK filesystem")
		err, _, _ := runCmd("fsck", "-n", d.DevPath)
		if err == nil {
			return
		}
	}

	// attempt fix
	var fixCmd []string

	// Get fstype
	// err, fstype, _ := runCmd("blkid", "-o", "value", "-s", "TYPE", d.DevPath)
	// if err == nil {
	// 	fstype = strings.TrimSpace(string(fstype))
	// 	if fstype != "" {
	// 		fstype = strings.ToLower(strings.TrimSpace(fstype))
	// 		switch fstype {
	// 		case "vfat", "fat", "fat12", "fat16", "fat32", "msdos":
	// 			fixCmd = []string{"fsck",}
	// 		case "ext2", "ext3", "ext4":
	// 			fallthrough
	// 		default:
	// 			break
	// 		}
	// 	}
	// } else {
	// 	util.Log.Error("blkid failed", "err", err)
	// }
	//
	// -f forces a check even when the superblock says clean, but not every
	// checker accepts it (fsck.vfat has no -f), so fall back to a plain repair.
	attempts := [][]string{{"fsck", "-y", d.DevPath}}
	if force {
		attempts = [][]string{{"fsck", "-f", "-y", d.DevPath}, {"fsck", "-y", d.DevPath}}
	}
	for _, fixCmd = range attempts {
		util.Log.Info("fsck", "command", fixCmd)
		err, stdout, stderr := runCmd(fixCmd...)
		util.Log.Info("fsck", "stdout", stdout)
		util.Log.Info("fsck", "stderr", stderr)
		if err == nil {
			return
		}
		util.Log.Error("Error checking filesystem:", "err", err)
	}
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

	d.MaybeRepair(false)

	// REVISIT: consider mounting with -o sync to reduce chance of dirty filesystem if media removed.
	err, stdout, stderr := runCmd("mount" /* "-o", "sync", */, d.DevPath, d.HostMountpoint())

	if err != nil {
		util.Log.Error("mount failed", "dev", d.DevPath, "mp", d.HostMountpoint(), "err", err)
		util.Log.Error(fmt.Sprintf("stdout: %s", stdout))
		util.Log.Error(fmt.Sprintf("stderr: %s", stderr))
		return ""
	}

	util.Log.Info("MOUNTED", "dev", d.DevPath, "mp", d.Mountpoint())
	util.Log.Error(fmt.Sprintf("stdout: %s", stdout))
	util.Log.Error(fmt.Sprintf("stderr: %s", stderr))
	return d.Mountpoint()
}

// ClearStaleMount unmounts anything still occupying d's canonical mountpoint
// that isn't d itself, and is a no-op when the mountpoint is free.
func (d Device) ClearStaleMount() {
	source, mounted := d.MountInfo()
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

func (d Device) EnsureMounted() bool {
	mountRetries := 3
	if d.IsMounted() {
		return d.repairIfReadOnly()
	}
	for i := 0; i < mountRetries; i++ {
		d.Mount()
		if d.IsMounted() {
			return d.repairIfReadOnly()
		}
		util.Log.Error("mounting failed", "attempt", i, "device", d)
		time.Sleep(1 * time.Second)
	}
	util.Log.Info("trying to clear any stale mountpoint and retry", "device", d)
	d.ClearStaleMount()
	d.Mount()
	if d.IsMounted() {
		return d.repairIfReadOnly()
	}
	util.Log.Error("** final attempt to mount failed **", "device", d)
	return false
}

// repairIfReadOnly deals with a device that mounted but came up read-only: the
// mount worked and the fsck during it reported nothing, yet the kernel still
// won't write to the filesystem. Unmount, force a repair, mount again. Reports
// whether the device ended up usable, i.e. mounted and writable.
func (d Device) repairIfReadOnly() bool {
	if !d.IsReadOnly() {
		return true
	}
	util.Log.Error("mounted READ ONLY, forcing repair and remounting", "device", d, "mp", d.Mountpoint())

	if err := syscall.Unmount(d.HostMountpoint(), 0); err != nil {
		util.Log.Error("unmount before forced repair failed", "path", d.HostMountpoint(), "err", err)
		return false
	}
	d.MaybeRepair(true)
	d.Mount()

	if !d.IsMounted() {
		util.Log.Error("remount after forced repair failed", "device", d)
		return false
	}
	if d.IsReadOnly() {
		util.Log.Error("** still mounted READ ONLY after forced repair **", "device", d)
		return false
	}
	util.Log.Info("remounted read-write after forced repair", "device", d)
	return true
}

// Discover walks hostRoot's /dev/disk/by-id for usb-...-partN symlinks and
// resolves manufacturer/product/serial via sysfs.
func Discover(hostRoot string) []Device {
	fileContents := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}

	// usbDir walks sysfs up from the block device until it finds a directory
	// containing a "serial" file — that is the USB device node carrying
	// manufacturer/product/serial attributes.
	usbDir := func(device, sysClassBlock string) string {
		base := filepath.Base(device)
		d, err := filepath.EvalSymlinks(filepath.Join(sysClassBlock, base))
		if err != nil {
			return ""
		}
		for {
			if _, err := os.Stat(filepath.Join(d, "serial")); err == nil {
				return d
			}
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
		return ""
	}

	diskById := filepath.Join(hostRoot, "/dev/disk/by-id")

	diskEntries, err := os.ReadDir(diskById) // already sorted by name
	if err != nil {
		// Assume transient failure.
		util.Log.Info("cannot read device dir", "path", diskById, "err", err)
		return nil
	}

	var devs []Device
	usbPartRegexp := regexp.MustCompile(`^usb-.+-part(\d+)$`)
	for _, e := range diskEntries {
		if m := usbPartRegexp.FindStringSubmatch(e.Name()); m != nil {
			partition, _ := strconv.Atoi(m[1])

			devicePath, err := filepath.EvalSymlinks(filepath.Join(diskById, e.Name()))
			if err != nil {
				util.Log.Debug("filepath.EvalSymlinks", "err", err)
				continue
			}

			usbNode := usbDir(devicePath, filepath.Join(hostRoot, "/sys/class/block"))
			if usbNode == "" {
				util.Log.Info("no USB sysfs node", "device", devicePath)
				continue
			}

			devs = append(devs, Device{
				Manufacturer: fileContents(filepath.Join(usbNode, "manufacturer")),
				Model:        fileContents(filepath.Join(usbNode, "product")),
				Serial:       fileContents(filepath.Join(usbNode, "serial")),
				Partition:    partition,
				DevPath:      devicePath,
				HostRoot:     hostRoot,
			})
		}
	}
	return devs
}

// DiscoverMounted returns only the discovered devices that are mounted.
func DiscoverMounted(hostRoot string) []Device {
	var out []Device
	for _, d := range Discover(hostRoot) {
		if d.IsMounted() {
			out = append(out, d)
		}
	}
	return out
}

// MatchVolumeContext returns the subset of devices satisfying every topology
// segment in vc whose key starts with driverName+"/".
func MatchVolumeContext(devices []Device, driverName string, vc map[string]string) []Device {
	prefix := driverName + "/"
	var matched []Device
	for _, dev := range devices {
		label := dev.Label()
		ok := true
		for k, v := range vc {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			if k[len(prefix):] != label || v != "true" {
				ok = false
				break
			}
		}
		if ok {
			matched = append(matched, dev)
		}
	}
	return matched
}
