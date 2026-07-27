// Package devicescan provides read-only discovery of USB block devices, the
// naming rules derived from them, and the shared answer to "is this device
// mounted where we expect it?". It has no side effects — no mounting, no
// formatting — so pkg/manager (which owns mounting) and pkg/driver (which only
// consumes what the manager prepared) can agree on device state without one
// depending on the other. That agreement matters: if the two disagree about
// whether a device is ready, the manager labels the node while the driver
// refuses to publish, and pods schedule somewhere they cannot mount.
package devicescan

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

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
}

func (d Device) String() string {
	partition := strconv.Itoa(d.Partition)
	if d.Partition == 0 {
		partition = "??"
	}
	return fmt.Sprintf("serial: %s, manufacturer: %q, type: usb, model: %q, partition: %s", d.Serial, d.Manufacturer, d.Model, partition)
}

// DeviceLabel builds the canonical "<manufacturer>-<model>" topology name
// for a device, e.g. "sandisk-cruzer".
func DeviceLabel(manufacturer, model string) string {

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

// Mountpoint returns the host-absolute mountpoint a device with the
// given serial should be mounted at.
func Mountpoint(serial string) string {
	return fmt.Sprintf("/mnt/k8s-freeport-%s", serial)
}

// usbNodeFor walks sysfs up from the block device until it finds a directory
// containing a "serial" file — that is the USB device node carrying
// manufacturer/product/serial attributes.
func usbNodeFor(device, sysClassBlock string) string {

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

// Discover walks hostRoot's /dev/disk/by-id for usb-...-partN symlinks and
// resolves manufacturer/product/serial via sysfs.
func Discover(hostRoot string) []Device {
	readSysAttr := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
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

			device, err := filepath.EvalSymlinks(filepath.Join(diskById, e.Name()))
			if err != nil {
				util.Log.Debug("filepath.EvalSymlinks", "err", err)
				continue
			}

			usbNode := usbNodeFor(device, filepath.Join(hostRoot, "/sys/class/block"))
			if usbNode == "" {
				util.Log.Info("no USB sysfs node", "device", device)
				continue
			}

			devs = append(devs, Device{
				Manufacturer: readSysAttr(filepath.Join(usbNode, "manufacturer")),
				Model:        readSysAttr(filepath.Join(usbNode, "product")),
				Serial:       readSysAttr(filepath.Join(usbNode, "serial")),
				Partition:    partition,
				DevPath:      device,
			})
		}
	}
	return devs
}

// MountedAt parses hostRoot's /proc/1/mounts and reports which source device
// is mounted at dev's canonical mountpoint, if anything is.
func MountedAt(hostRoot string, dev Device) (source string, mounted bool) {
	f, err := os.Open(filepath.Join(hostRoot, "/proc/1/mounts"))
	if err != nil {
		util.Log.Debug("cannot read mount table", "err", err)
		return "", false
	}
	defer f.Close()

	// Applied to both sides, so it doesn't matter which form either started in.
	bare := func(p string) string { return strings.TrimPrefix(p, hostRoot) }
	want := bare(Mountpoint(dev.Serial))

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) >= 2 && bare(parts[1]) == want {
			source, mounted = bare(parts[0]), true
		}
	}
	return source, mounted
}

// IsMounted reports whether dev itself is currently mounted at its canonical
// mountpoint.
func IsMounted(hostRoot string, dev Device) bool {
	source, mounted := MountedAt(hostRoot, dev)
	return mounted && source == strings.TrimPrefix(dev.DevPath, hostRoot)
}
