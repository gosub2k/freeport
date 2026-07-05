package driver

import (
	"bufio"
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

// usb-...-partN identifies a USB partition symlink; partition number in group 1.
var usbPartRegexp = regexp.MustCompile(`^usb-.+-part(\d+)$`)

type hostBlockDevice struct {
	manufacturer string
	model        string
	serial       string
	partition    int
	free         int64
	mountpoint   string // absolute path on host
}

func (d hostBlockDevice) String() string {
	partition := strconv.Itoa(d.partition)
	if d.partition == 0 {
		partition = "??"
	}
	return fmt.Sprintf("serial: %s, manufacturer: %q, type: usb, model: %q, partition: %s", d.serial, d.manufacturer, d.model, partition)
}

// sanitize lowercases s and replaces any character that is not alphanumeric,
// '-', '_', or '.' with '-', then strips leading/trailing separators.
func sanitize(s string) string {
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

// deviceClassNameLimit is the Kubernetes label-name length limit (63 chars),
// which topology keys and volume-context attribute names must also satisfy.
const deviceClassNameLimit = 63

// deviceClassKey builds the canonical "<manufacturer>-<model>" topology name
// for a device, e.g. "sandisk-cruzer". A missing manufacturer or model is
// replaced with "unknown" rather than collapsing into a bare separator. If
// the combined length would exceed deviceClassNameLimit, both halves are
// truncated to an even split so neither one starves the other.
func deviceClassKey(manufacturer, model string) string {
	m := sanitize(manufacturer)
	if m == "" {
		m = "unknown"
	}
	d := sanitize(model)
	if d == "" {
		d = "unknown"
	}

	const sep = "-"
	budget := deviceClassNameLimit - len(sep)
	if len(m)+len(d) > budget {
		half := budget / 2
		m = m[:min(len(m), half)]
		d = d[:min(len(d), budget-len(m))]
		m = strings.TrimRight(m, "-_.")
		d = strings.TrimRight(d, "-_.")
	}
	return m + sep + d
}

// matchVolumeContext returns the subset of devices whose "<manufacturer>-<model>"
// class key satisfies every topology segment in vc whose key starts with
// driverName+"/". Segments are expected in the form
// "<driverName>/<manufacturer>-<model>=true".
func matchVolumeContext(devices []hostBlockDevice, driverName string, vc map[string]string) []hostBlockDevice {
	prefix := driverName + "/"
	var matched []hostBlockDevice
	for _, dev := range devices {
		classKey := deviceClassKey(dev.manufacturer, dev.model)
		ok := true
		for k, v := range vc {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			if k[len(prefix):] != classKey || v != "true" {
				ok = false
			}
			if !ok {
				break
			}
		}
		if ok {
			matched = append(matched, dev)
		}
	}
	return matched
}

func readSysAttr(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
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

func getDf(mountpoint string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mountpoint, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * st.Bsize
}

// mountDevice ensures devPath is mounted at /mnt/k8s-freeport-<serial> on the
// host. Returns the host-absolute mountpoint, or "" on failure.
func mountDevice(hostRoot, devPath, serial string) string {
	mountpoint := fmt.Sprintf("/mnt/k8s-freeport-%s", serial)

	// check the host's active mounts before attempting a new one
	procMounts := filepath.Join(hostRoot, "/proc/1/mounts")
	if f, err := os.Open(procMounts); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			parts := strings.Fields(sc.Text())
			if len(parts) >= 2 && parts[0] == devPath {
				f.Close()
				return parts[1]
			}
		}
		f.Close()
	}

	hostMountpoint := filepath.Join(hostRoot, mountpoint)
	if err := os.MkdirAll(hostMountpoint, 0750); err != nil {
		util.Log.Error("mkdir failed", "path", hostMountpoint, "err", err)
		return ""
	}
	if out, err := exec.Command("mount", devPath, hostMountpoint).CombinedOutput(); err != nil {
		util.Log.Error("mount failed", "dev", devPath, "mp", hostMountpoint, "err", err, "output", strings.TrimSpace(string(out)))
		return ""
	}
	util.Log.Info("mounted", "dev", devPath, "mp", mountpoint)
	return mountpoint
}

// scanUSBDevices discovers USB block device partitions via /dev/disk/by-id,
// mounts each one, and returns only the successfully mounted devices.
func scanUSBDevices(hostRoot string) []hostBlockDevice {
	devDir := filepath.Join(hostRoot, "/dev/disk/by-id")
	sysClassBlock := filepath.Join(hostRoot, "/sys/class/block")

	entries, err := os.ReadDir(devDir) // already sorted by name
	if err != nil {
		util.Log.Info("cannot read device dir", "path", devDir, "err", err)
		return nil
	}

	var devs []hostBlockDevice
	for _, e := range entries {
		m := usbPartRegexp.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		partition, _ := strconv.Atoi(m[1])

		device, err := filepath.EvalSymlinks(filepath.Join(devDir, e.Name()))
		if err != nil {
			continue
		}

		usbNode := usbNodeFor(device, sysClassBlock)
		if usbNode == "" {
			util.Log.Info("no USB sysfs node", "device", device)
			continue
		}

		serial := readSysAttr(filepath.Join(usbNode, "serial"))
		mountpoint := mountDevice(hostRoot, device, serial)
		if mountpoint == "" {
			continue // not mounted — excluded from topology
		}

		hostMP := filepath.Join(hostRoot, mountpoint)
		devs = append(devs, hostBlockDevice{
			manufacturer: readSysAttr(filepath.Join(usbNode, "manufacturer")),
			model:        readSysAttr(filepath.Join(usbNode, "product")),
			serial:       serial,
			partition:    partition,
			free:         getDf(hostMP),
			mountpoint:   mountpoint,
		})
	}
	return devs
}
