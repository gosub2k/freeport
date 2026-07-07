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
}

func New(clientset kubernetes.Interface, nodeName, hostRoot, driverName string) *Manager {
	return &Manager{
		clientset:  clientset,
		nodeName:   nodeName,
		hostRoot:   hostRoot,
		driverName: driverName,
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
	var mounted []mountedDevice
	for _, d := range discovered {
		mp := mountDevice(m.hostRoot, d)
		if mp == "" {
			continue
		}
		mounted = append(mounted, mountedDevice{
			Device:     d,
			mountpoint: mp,
			free:       getDf(mp),
		})
	}
	for _, d := range mounted {
		util.Log.Info(d.String())
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

	util.Log.Info("patching node labels", "node", m.nodeName, "patch", patch)
	_, err = m.clientset.CoreV1().Nodes().Patch(ctx, m.nodeName, types.MergePatchType, body, metav1.PatchOptions{})
	return err
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
