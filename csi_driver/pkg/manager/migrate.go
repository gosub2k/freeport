// Device migration: when a device reappears on this node after having been
// unplugged from another, the volumes on it still belong (per their PV's
// immutable nodeAffinity) to whichever node they were last provisioned on.
// This file detects that mismatch — via the pods actually using the volume,
// not the PV's own stale nodeAffinity — and repairs it by deleting and
// recreating the PV with this node's identity, then bouncing the pod so its
// controller reschedules it here. PV nodeAffinity is immutable (pre-1.35,
// and the MutablePVNodeAffinity alpha gate is deliberately not used here),
// so delete+recreate is the only way to repoint it.
package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"freeport/pkg/util"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const pvProtectionFinalizer = "kubernetes.io/pv-protection"
const hostnameTopologyKey = "kubernetes.io/hostname"

// migrateStaleVolumes scans every mounted device's volume directories and,
// for any that belong to a Bound, Retain-policy PV whose claim's pod is
// running on a different node, deletes and recreates that PV pinned to this
// node and deletes the pod so its controller can reschedule it here. Per-item
// errors are logged and never abort the rest of the pass, matching
// ReconcileOnce's existing "log and continue" idiom.
func (m *Manager) migrateStaleVolumes(ctx context.Context, mounted []mountedDevice) error {
	if len(mounted) == 0 {
		return nil
	}

	pvsByHandle, err := m.listPVsByVolumeHandle(ctx)
	if err != nil {
		return err
	}

	podsByNamespace := map[string][]corev1.Pod{}

	for _, d := range mounted {
		dirs, err := volumeDirs(m.hostRoot, d.mountpoint)
		if err != nil {
			util.Log.Error("cannot list volume dirs", "mountpoint", d.mountpoint, "err", err)
			continue
		}
		for _, dirName := range dirs {
			pv, ok := pvsByHandle[dirName]
			if !ok {
				continue // no PV claims this directory (e.g. lost+found) — not ours
			}
			if !eligiblePV(pv) {
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

			newPV, err := m.recreatePVForNode(ctx, pv)
			if err != nil {
				util.Log.Error("PV recreation failed", "pv", pv.Name, "err", err)
				continue
			}
			util.Log.Info("recreated PV for migrated device", "pv", newPV.Name)

			for i := range toBounce {
				if err := m.bouncePod(ctx, &toBounce[i]); err != nil {
					util.Log.Error("bouncing pod failed", "pod", toBounce[i].Namespace+"/"+toBounce[i].Name, "err", err)
				}
			}
		}
	}
	return nil
}

// listPVsByVolumeHandle indexes every PV owned by this driver by its CSI
// volume handle, so each device's volume directories can be matched with a
// single map lookup instead of a List call per directory.
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

// volumeDirs lists the immediate subdirectory names of a mounted device's
// mountpoint — each one is a candidate volumeID, per the 1-1 correspondence
// NodePublishVolume maintains between a volume and its directory.
func volumeDirs(hostRoot, mountpoint string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(hostRoot, mountpoint))
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs, nil
}

// eligiblePV reports whether pv is safe to migrate: Bound to a claim, and
// Retain-policy (a Delete-policy PV is unsafe to delete/recreate here since
// the external-provisioner could race a real DeleteVolume call against the
// brief window the PV object doesn't exist).
func eligiblePV(pv *corev1.PersistentVolume) bool {
	if pv.Status.Phase != corev1.VolumeBound {
		util.Log.Debug("skipping migration, PV not bound", "pv", pv.Name, "phase", pv.Status.Phase)
		return false
	}
	if pv.Spec.ClaimRef == nil {
		util.Log.Debug("skipping migration, PV has no claimRef", "pv", pv.Name)
		return false
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		util.Log.Debug("skipping migration, PV reclaim policy is not Retain", "pv", pv.Name, "policy", pv.Spec.PersistentVolumeReclaimPolicy)
		return false
	}
	return true
}

func (m *Manager) listPods(ctx context.Context, namespace string) ([]corev1.Pod, error) {
	list, err := m.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// podsToBounce returns the pods among pods that mount claimName and are
// scheduled to a node other than thisNode. Pods already terminating, not yet
// scheduled, or already on thisNode are excluded — this is what makes
// re-running the scan every reconcile tick a safe no-op once nothing is
// actually mismatched.
func podsToBounce(pods []corev1.Pod, claimName, thisNode string) []corev1.Pod {
	var out []corev1.Pod
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Spec.NodeName == "" || pod.Spec.NodeName == thisNode {
			continue
		}
		if !podMountsClaim(&pod, claimName) {
			continue
		}
		out = append(out, pod)
	}
	return out
}

func podMountsClaim(pod *corev1.Pod, claimName string) bool {
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == claimName {
			return true
		}
	}
	return false
}

// recreatePVForNode deletes pv and recreates it under the same name with
// NodeAffinity mutated to require this node, preserving everything else
// (including ClaimRef, so the existing PVC rebinds by name automatically).
// pv must already have passed eligiblePV.
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

// pinNodeAffinity adds a kubernetes.io/hostname requirement to pv's first
// NodeSelectorTerm (AND'd alongside whatever's already there, e.g. the
// device-class expression from the original provisioning) — a new term
// would OR rather than further constrain, which is wrong here. Any
// pre-existing hostname requirement (from an earlier migration to a
// different node) is replaced rather than appended to, so repeated
// migrations don't accumulate a self-contradictory "AND of two different
// single-host In clauses" that would match nothing.
func pinNodeAffinity(pv *corev1.PersistentVolume, nodeName string) {
	req := corev1.NodeSelectorRequirement{
		Key:      hostnameTopologyKey,
		Operator: corev1.NodeSelectorOpIn,
		Values:   []string{nodeName},
	}

	if pv.Spec.NodeAffinity == nil {
		pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{}
	}
	if pv.Spec.NodeAffinity.Required == nil || len(pv.Spec.NodeAffinity.Required.NodeSelectorTerms) == 0 {
		pv.Spec.NodeAffinity.Required = &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{}},
		}
	}

	term := &pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0]
	filtered := term.MatchExpressions[:0]
	for _, existing := range term.MatchExpressions {
		if existing.Key != hostnameTopologyKey {
			filtered = append(filtered, existing)
		}
	}
	term.MatchExpressions = append(filtered, req)
}

// bouncePod deletes pod gracefully (so kubelet completes the normal volume
// teardown — NodeUnpublishVolume — on its current node before the object is
// removed) so its controller recreates it and the scheduler can place it on
// this node. A pod with no owning controller is left alone: nothing would
// recreate it, and a Pod's node placement can never change in place, so
// deleting it would just destroy the workload with no path back.
func (m *Manager) bouncePod(ctx context.Context, pod *corev1.Pod) error {
	if len(pod.OwnerReferences) == 0 {
		util.Log.Error("bare pod using migrated device, refusing to delete — manual intervention required",
			"pod", pod.Namespace+"/"+pod.Name, "podNode", pod.Spec.NodeName)
		return nil
	}
	return m.clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
}
