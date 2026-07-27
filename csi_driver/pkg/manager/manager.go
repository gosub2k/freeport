// Package manager is glue code between the driver & controller (pure CSI) and Kubernetes.
// It has FIVE functions:
// - scan for devices on the node
// - mount devices onto a consistently named mountpoint
// - label Node object with devices that are found so that the CSI part will respect the topology constrains in the storage class
// - act as a migration controller. If it finds a volume directory on device mounted on the node, it will (when appropriate):
//   - check for pvcs using that pv and any corresponding pods
//   - recreate the pv, setting node affinity to this node
//   - delete any pods that are scheduled on a different node and that mount those pvcs
//     The migration process is intended to allow, for example, StatefulSets using persistent storage to have the storage physically moved to a different node without affecting the number of running replicas.
//
// - sync the topology keys on the CSINode object with the devices that are currently on the node. This doesn't happen naturally because k8s only calls GetNodeInfo RPC once on each node when the socket is created.
// REVISIT: combining these functions into one process or splitting up. There are already several reconcile loops so perhaps its best to aggregate these functions.
package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"freeport/pkg/device"
	"freeport/pkg/util"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Utility functions:
/////////////////////

// deviceDelta returns the devices added and removed.
func deviceDelta(prev, current map[string]device.Device) (added, removed []device.Device) {
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

// desiredLabels returns the full set of device labels this Node should carry
func desiredLabels(driverName string, devices []device.Device) map[string]string {
	labels := map[string]string{}
	for _, d := range devices {
		key := driverName + "/" + d.Label()
		labels[key] = "true"
	}
	return labels
}

// nodeLabelPatch compares a node's current labels against the desired set and
// returns a patch: new/changed keys map to their desired value, and any
// existing key under prefix that's absent from desired maps to nil (deletes it)
func nodeLabelPatch(current, desired map[string]string, prefix string) map[string]any {
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

// Manager struct:
//////////////////

type Manager struct {
	clientset  kubernetes.Interface
	nodeName   string
	hostRoot   string
	driverName string

	// lastSeen is the mounted-device set from the previous ReconcileOnce
	// call, keyed by serial,.
	lastSeen map[string]device.Device

	// mounter mounts discovered devices and tracks their mount failures.
	mounter *device.Mounter
}

// New returns a new Manager.
func New(clientset kubernetes.Interface, nodeName, hostRoot, driverName string) *Manager {
	return &Manager{
		clientset:  clientset,
		nodeName:   nodeName,
		hostRoot:   hostRoot,
		driverName: driverName,
		lastSeen:   map[string]device.Device{},
		mounter:    device.NewMounter(),
	}
}

// listPVsByVolumeHandle return map of CSI VolumeHandle -> PersistentVolume object
func (m *Manager) listPVsByVolumeHandle(ctx context.Context) (map[string]*corev1.PersistentVolume, error) {
	list, err := m.clientset.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	byHandle := map[string]*corev1.PersistentVolume{}
	for i := range list.Items {
		pv := &list.Items[i]
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != m.driverName {
			continue
		}
		byHandle[pv.Spec.CSI.VolumeHandle] = pv
	}
	return byHandle, nil
}

// recreatePVForNode recreates PV with mutated node affinity, ie to the node this instance is running on.
func (m *Manager) recreatePVForNode(ctx context.Context, pv *corev1.PersistentVolume) (*corev1.PersistentVolume, error) {
	pvs := m.clientset.CoreV1().PersistentVolumes()

	fresh, err := pvs.Get(ctx, pv.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("re-fetching PV %s before finalizer strip: %w", pv.Name, err)
	}
	var kept []string
	for _, f := range fresh.Finalizers {
		if f != pvProtectionFinalizer {
			kept = append(kept, f)
		}
	}
	fresh.Finalizers = kept
	fresh, err = pvs.Update(ctx, fresh, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("stripping finalizer on PV %s: %w", pv.Name, err)
	}

	newPV := fresh.DeepCopy()
	newPV.ResourceVersion = ""
	newPV.UID = ""
	newPV.CreationTimestamp = metav1.Time{}
	newPV.ManagedFields = nil
	newPV.Finalizers = nil
	newPV.Generation = 0
	newPV.Status = corev1.PersistentVolumeStatus{}
	pinNodeAffinity(newPV, m.nodeName)

	if err := pvs.Delete(ctx, pv.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("deleting PV %s: %w", pv.Name, err)
	}
	created, err := pvs.Create(ctx, newPV, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("recreating PV %s: %w", pv.Name, err)
	}
	return created, nil
}

// bouncePod deletes pod gracefully so kubelet completes the normal volume
// teardown with NodeUnpublishVolume on current node before object removed.
func (m *Manager) bouncePod(ctx context.Context, pod *corev1.Pod) error {
	if len(pod.OwnerReferences) == 0 {
		// There is no controller so decide to leave it alone - nothing will recreate it.
		// REVISIT: the manager here could here act as the controller.
		util.Log.Error("bare pod using migrated device, refusing to delete — manual intervention required",
			"pod", pod.Namespace+"/"+pod.Name, "podNode", pod.Spec.NodeName)
		return nil
	}
	return m.clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
}

// listPods returns a list of all the pods in the given namespace.
func (m *Manager) listPods(ctx context.Context, namespace string) ([]corev1.Pod, error) {
	list, err := m.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// migrateStaleVolumes scans every mounted device's volume directories and,
// for any that belong to a Bound, Retain-policy PV whose claim's pod is
// running on a different node, deletes and recreates that PV pinned to this
// node and deletes the pod so its controller can reschedule it here.
func (m *Manager) migrateStaleVolumes(ctx context.Context, mounted []device.Device) error {
	if len(mounted) == 0 {
		return nil
	}

	pvsByHandle, err := m.listPVsByVolumeHandle(ctx)
	if err != nil {
		return err
	}

	podsByNamespace := map[string][]corev1.Pod{}

	for _, d := range mounted {
		dirs, err := volumeDirs(m.hostRoot, d.Mountpoint())
		if err != nil {
			util.Log.Error("cannot list volume dirs", "mountpoint", d.Mountpoint(), "err", err)
			continue
		}
		for _, dirName := range dirs {
			pv, ok := pvsByHandle[dirName]
			if !ok {
				continue // no PV claims this directory (e.g. lost+found) — not ours
			}
			if !isOkToMigrate(pv) {
				continue // eligiblePV already logged why
			}

			ns, claimName := pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name
			pods, ok := podsByNamespace[ns]
			if !ok {
				pods, err = m.listPods(ctx, ns)
				if err != nil {
					util.Log.Error("listing pods for claim failed", "namespace", ns, "claim", claimName, "err", err)
					continue
				}
				podsByNamespace[ns] = pods
			}

			toBounce := podsToBounce(pods, claimName, m.nodeName)
			if len(toBounce) == 0 {
				continue // already correctly placed, pending, or terminating — no-op
			}

			util.Log.Info("RECREATING PV on this node", "pv", pv.Name)
			newPV, err := m.recreatePVForNode(ctx, pv)
			if err != nil {
				util.Log.Error("PV recreation failed", "pv", pv.Name, "err", err)
				continue
			}
			util.Log.Info("recreated", "pv", newPV.Name)

			for i := range toBounce {
				util.Log.Info("DELETE pod so it can be recreated by controller", "pv", pv.Name, "pod", toBounce[i])
				if err := m.bouncePod(ctx, &toBounce[i]); err != nil {
					util.Log.Error("bouncing pod failed", "pod", toBounce[i].Namespace+"/"+toBounce[i].Name, "err", err)
				}
			}
		}
	}
	return nil
}

// syncTopologyKeys keeps this node's CSINode driver entry's topologyKeys in
// sync with whichever devices are currently mounted. It recreates the CSINode
// object because the TopologyKeys part is immutable.
func (m *Manager) syncTopologyKeys(ctx context.Context, mounted []device.Device) error {
	csiNode, err := m.clientset.StorageV1().CSINodes().Get(ctx, m.nodeName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil // node-driver-registrar hasn't registered this node yet
		}
		return err
	}

	idx := driverIndex(csiNode.Spec.Drivers, m.driverName)
	if idx == -1 {
		return nil // registrar hasn't registered this driver on this node yet
	}

	desired := desiredTopologyKeys(m.driverName, mounted)
	if topologyKeysEqual(csiNode.Spec.Drivers[idx].TopologyKeys, desired) {
		return nil
	}

	newCSINode := csiNode.DeepCopy()
	newCSINode.Spec.Drivers[idx].TopologyKeys = desired
	newCSINode.ResourceVersion = ""
	newCSINode.UID = ""
	newCSINode.CreationTimestamp = metav1.Time{}
	newCSINode.ManagedFields = nil
	newCSINode.Finalizers = nil
	newCSINode.Generation = 0

	csiNodes := m.clientset.StorageV1().CSINodes()
	if err := csiNodes.Delete(ctx, m.nodeName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting CSINode %s: %w", m.nodeName, err)
	}
	util.Log.Info("recreating CSINode with updated topology keys", "keys", desired)
	_, err = csiNodes.Create(ctx, newCSINode, metav1.CreateOptions{})
	return err
}

// ReconcileOnce scans for devices, mounts any that aren't already mounted,
// and patches this node's labels to reflect exactly the set of device
// classes found.
func (m *Manager) ReconcileOnce(ctx context.Context) error {

	// Find eligible devices and make sure they are mounted.
	discovered := device.Discover(m.hostRoot)
	mounted := m.mounter.EnsureMounted(discovered)
	// Log changes:
	current := map[string]device.Device{} // Serial -> device.Device
	for _, d := range mounted {
		current[d.Serial] = d
	}
	added, removed := deviceDelta(m.lastSeen, current)
	for _, d := range added {
		util.Log.Info("device ADDED", "manufacturer", d.Manufacturer, "model", d.Model, "mountpoint", d.Mountpoint())
	}
	for _, d := range removed {
		util.Log.Info("device REMOVED", "manufacturer", d.Manufacturer, "model", d.Model, "mountpoint", d.Mountpoint())
		d.Unmount()
	}
	m.lastSeen = current

	// Migrate volumes.
	if err := m.migrateStaleVolumes(ctx, mounted); err != nil {
		util.Log.Error("volume migration failed", "err", err)
	}

	// Update topology keys on CSINode objects
	if err := m.syncTopologyKeys(ctx, mounted); err != nil {
		util.Log.Error("syncing CSINode topology keys failed", "err", err)
	}

	// Update labels on the Node object
	node, err := m.clientset.CoreV1().Nodes().Get(ctx, m.nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	desired := desiredLabels(m.driverName, mounted)
	patch := nodeLabelPatch(node.Labels, desired, m.driverName+"/")
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

// Run reconciles on every tick until ctx is cancelled.
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
