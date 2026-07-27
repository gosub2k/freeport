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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

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

// IsSwap reports whether d is formatted as Linux swap.
func (d Device) IsSwap() bool {
	// Sysfs doesn't report fs type if its not mounted, use command.
	out, err := exec.Command("blkid", "-o", "value", "-s", "TYPE", d.DevPath).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "swap"
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

// maxMountFailures caps how many times a Mounter retries one device node.
const maxMountFailures = 3

// Mounter mounts discovered devices, remembering per-device-node failures so a
// device that cannot be mounted is not retried on every pass forever.
type Mounter struct {
	// failures counts consecutive mount(8) failures per device node. Keyed by
	// node rather than serial because the partitions of one stick share a
	// serial: keying by serial let a good partition's success clear its broken
	// sibling's count every pass, so the cap never engaged.
	failures map[string]int

	// mountFn is Device.Mount by default; swappable in tests.
	mountFn func(Device) string
}

func NewMounter() *Mounter {
	return &Mounter{failures: map[string]int{}, mountFn: Device.Mount}
}

// EnsureMounted makes sure every discovered device is mounted where it
// belongs, and returns those that are.
func (mo *Mounter) EnsureMounted(discovered []Device) []Device {
	seen := map[string]bool{}
	var mounted []Device
	for _, d := range discovered {
		seen[d.DevPath] = true
		if d.IsMounted() {
			mounted = append(mounted, d)
			continue
		}
		if mo.failures[d.DevPath] >= maxMountFailures {
			continue
		}
		// Not mounted, but the mountpoint may still be held by the node this
		// device had before it was unplugged and replugged.
		d.ClearStaleMount()
		if mo.mountFn(d) == "" {
			mo.failures[d.DevPath]++
			if mo.failures[d.DevPath] == maxMountFailures {
				util.Log.Error("mount failed repeatedly, giving up until device is removed and reinserted",
					"dev", d.DevPath, "failures", maxMountFailures)
			}
			continue
		}
		delete(mo.failures, d.DevPath)
		mounted = append(mounted, d)
	}
	// Forget devices that are no longer present, so a reinserted device gets a
	// fresh attempt budget.
	for devPath := range mo.failures {
		if !seen[devPath] {
			delete(mo.failures, devPath)
		}
	}
	return mounted
}
