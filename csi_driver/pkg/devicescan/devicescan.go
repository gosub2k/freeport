// Package devicescan owns USB block devices: discovering them via sysfs, the
// naming rules derived from them, and (in mount.go) mounting them and
// answering "is this device mounted where we expect it?".
//
// It is the one place both pkg/manager (which mounts devices) and pkg/driver
// (which publishes what the manager prepared) get their answers from, so the
// two cannot drift apart. That matters: when they disagree about whether a
// device is ready, the manager labels the node while the driver refuses to
// publish, and pods schedule somewhere they cannot mount.
package devicescan

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"freeport/pkg/util"
)

const Maxk8sLabelLength = 63

// Device is a USB block device partition discovered via sysfs. It carries the
// hostRoot it was discovered under so its mount methods need no extra context.
type Device struct {
	Manufacturer string
	Model        string
	Serial       string
	Partition    int
	DevPath      string // real device node, e.g. "/dev/sdb1"

	// HostRoot is where the real host's filesystem is bind-mounted into this
	// container, e.g. "/host". Paths this process resolves carry it as a
	// prefix; paths the host's own PID 1 records never do.
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
func (d Device) Label() string {
	return DeviceLabel(d.Manufacturer, d.Model)
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
				HostRoot:     hostRoot,
			})
		}
	}
	return devs
}

// DiscoverMounted returns only the discovered devices that are actually
// mounted at their canonical mountpoint — the ones pkg/manager has finished
// preparing, and so the only ones pkg/driver may publish.
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
// segment in vc whose key starts with driverName+"/". Segments are expected in
// the form "<driverName>/<manufacturer>-<model>=true"; keys belonging to any
// other driver are ignored. A device carries exactly one label, so two
// distinct driver-prefixed keys can never both match.
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
