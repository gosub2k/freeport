package driver

import (
	"fmt"
	"strconv"
	"strings"

	"freeport/pkg/devicescan"
)

// hostBlockDevice is a USB device the node driver considers ready to use —
// one pkg/manager has already discovered and mounted. The driver never
// mounts or formats anything itself.
type hostBlockDevice struct {
	manufacturer string
	model        string
	serial       string
	partition    int
	mountpoint   string // absolute path on host
}

func (d hostBlockDevice) String() string {
	partition := strconv.Itoa(d.partition)
	if d.partition == 0 {
		partition = "??"
	}
	return fmt.Sprintf("serial: %s, manufacturer: %q, type: usb, model: %q, partition: %s", d.serial, d.manufacturer, d.model, partition)
}

// scanReadyDevices discovers USB devices via devicescan and returns only
// those already mounted at their canonical mountpoint — i.e. the ones
// pkg/manager has finished setting up. Devices manager hasn't gotten to yet
// are silently excluded rather than mounted here.
func scanReadyDevices(hostRoot string) []hostBlockDevice {
	var out []hostBlockDevice
	for _, d := range devicescan.Discover(hostRoot) {
		mp, ok := devicescan.MountedAt(hostRoot, d.DevPath)
		if !ok {
			continue
		}
		out = append(out, hostBlockDevice{
			manufacturer: d.Manufacturer,
			model:        d.Model,
			serial:       d.Serial,
			partition:    d.Partition,
			mountpoint:   mp,
		})
	}
	return out
}

// matchVolumeContext returns the subset of devices whose "<manufacturer>-<model>"
// class key satisfies every topology segment in vc whose key starts with
// driverName+"/". Segments are expected in the form
// "<driverName>/<manufacturer>-<model>=true".
func matchVolumeContext(devices []hostBlockDevice, driverName string, vc map[string]string) []hostBlockDevice {
	prefix := driverName + "/"
	var matched []hostBlockDevice
	for _, dev := range devices {
		classKey := devicescan.DeviceClassKey(dev.manufacturer, dev.model)
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
