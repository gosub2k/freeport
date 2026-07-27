package manager

import (
	"freeport/pkg/device"
	"sort"

	storagev1 "k8s.io/api/storage/v1"
)

// desiredTopologyKeys returns the sorted set of topology keys this node's
// CSINode driver entry should declare.
func desiredTopologyKeys(driverName string, devices []device.Device) []string {
	labels := desiredLabels(driverName, devices)
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// driverIndex returns the index of the entry in drivers whose Name matches
// driverName, or -1 if this driver hasn't registered on this node yet.
func driverIndex(drivers []storagev1.CSINodeDriver, driverName string) int {
	for i, d := range drivers {
		if d.Name == driverName {
			return i
		}
	}
	return -1
}

// topologyKeysEqual reports whether two topology-key sets contain exactly
// the same keys, ignoring order.
func topologyKeysEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aSorted := append([]string(nil), a...)
	bSorted := append([]string(nil), b...)
	sort.Strings(aSorted)
	sort.Strings(bSorted)
	for i := range aSorted {
		if aSorted[i] != bSorted[i] {
			return false
		}
	}
	return true
}
