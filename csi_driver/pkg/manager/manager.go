// Package manager discovers USB block devices on a node, mounts them, and
// keeps that node's Kubernetes labels in sync with what's actually plugged
// in. It runs as its own DaemonSet, separate from and loosely coupled to the
// CSI node driver: the driver trusts that any device manager has recognized
// is already mounted and ready to use.
package manager

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"freeport/pkg/devicescan"
	"freeport/pkg/util"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

type Manager struct {
	clientset  kubernetes.Interface
	nodeName   string
	hostRoot   string
	driverName string

	// lastSeen is the mounted-device set from the previous ReconcileOnce
	// call, keyed by serial, purely so device add/remove transitions can be
	// logged at Info while steady-state presence stays at Debug. It carries
	// no correctness weight — losing it (e.g. on a manager restart) just
	// means the next tick logs every currently-mounted device as "added"
	// once, which is harmless.
	lastSeen map[string]mountedDevice

	// mountFailures counts consecutive mount(8) failures per device serial,
	// so mountAll can stop retrying (and stop logging) a permanently broken
	// device instead of failing identically forever. See mountAll.
	mountFailures map[string]int

	// mountFn is mountDevice by default; swappable in tests the same way
	// pkg/driver's NodeServer makes mountFn/scanFn swappable.
	mountFn func(hostRoot string, dev devicescan.Device) string
}

func New(clientset kubernetes.Interface, nodeName, hostRoot, driverName string) *Manager {
	return &Manager{
		clientset:     clientset,
		nodeName:      nodeName,
		hostRoot:      hostRoot,
		driverName:    driverName,
		lastSeen:      map[string]mountedDevice{},
		mountFailures: map[string]int{},
		mountFn:       mountDevice,
	}
}

// Run reconciles on every tick until ctx is cancelled. Errors are logged and
// do not stop the loop — a transient API-server hiccup shouldn't take the
// manager down.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := m.ReconcileOnce(ctx); err != nil {
		util.Log.Error("reconcile failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.ReconcileOnce(ctx); err != nil {
				util.Log.Error("reconcile failed", "err", err)
			}
		}
	}
}

// ReconcileOnce scans for devices, mounts any that aren't already mounted,
// and patches this node's labels to reflect exactly the set of device
// classes found — adding new ones and removing stale ones in the same pass.
func (m *Manager) ReconcileOnce(ctx context.Context) error {
	discovered := devicescan.Discover(m.hostRoot)
	mounted := m.mountAll(discovered)

	current := map[string]mountedDevice{}
	for _, d := range mounted {
		current[d.Serial] = d
		util.Log.Debug("mounted device", "manufacturer", d.Manufacturer, "model", d.Model, "mountpoint", d.mountpoint)
	}
	added, removed := deviceDelta(m.lastSeen, current)
	for _, d := range added {
		util.Log.Info("device added", "manufacturer", d.Manufacturer, "model", d.Model, "mountpoint", d.mountpoint)
	}
	for _, d := range removed {
		util.Log.Info("device removed", "manufacturer", d.Manufacturer, "model", d.Model, "mountpoint", d.mountpoint)
		cleanupMountpoint(m.hostRoot, d.mountpoint)
	}
	m.lastSeen = current

	if err := m.migrateStaleVolumes(ctx, mounted); err != nil {
		util.Log.Error("volume migration failed", "err", err)
	}

	node, err := m.clientset.CoreV1().Nodes().Get(ctx, m.nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	desired := desiredLabels(m.driverName, mounted)
	patch := diffLabels(node.Labels, desired, m.driverName+"/")
	if len(patch) == 0 {
		return nil
	}

	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"labels": patch},
	})
	if err != nil {
		return err
	}

	util.Log.Info("patching node labels", "patch", patch)
	_, err = m.clientset.CoreV1().Nodes().Patch(ctx, m.nodeName, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}

// deviceDelta returns the devices present in current but not prev (added)
// and vice versa (removed), keyed by serial, so ReconcileOnce can log only
// transitions at Info instead of every mounted device on every tick.
func deviceDelta(prev, current map[string]mountedDevice) (added, removed []mountedDevice) {
	for serial, d := range current {
		if _, ok := prev[serial]; !ok {
			added = append(added, d)
		}
	}
	for serial, d := range prev {
		if _, ok := current[serial]; !ok {
			removed = append(removed, d)
		}
	}
	return added, removed
}

// desiredLabels returns the full set of "<driverName>/<manufacturer>-<model>=true"
// labels this node should carry given the devices currently mounted.
func desiredLabels(driverName string, devices []mountedDevice) map[string]string {
	labels := map[string]string{}
	for _, d := range devices {
		key := driverName + "/" + devicescan.DeviceClassKey(d.Manufacturer, d.Model)
		labels[key] = "true"
	}
	return labels
}

// diffLabels compares a node's current labels against the desired set and
// returns a patch: new/changed keys map to their desired value, and any
// existing key under prefix that's absent from desired maps to nil (JSON
// merge-patch delete). Labels outside prefix are never touched.
func diffLabels(current, desired map[string]string, prefix string) map[string]any {
	patch := map[string]any{}
	for k, v := range desired {
		if current[k] != v {
			patch[k] = v
		}
	}
	for k := range current {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if _, ok := desired[k]; !ok {
			patch[k] = nil
		}
	}
	return patch
}
