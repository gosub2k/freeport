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
	"os"
	"path/filepath"

	"freeport/pkg/util"

	corev1 "k8s.io/api/core/v1"
)

const pvProtectionFinalizer = "kubernetes.io/pv-protection"
const hostnameTopologyKey = "kubernetes.io/hostname"

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

// isOkToMigrate reports whether we think its appropriate to migrate a persistent volume to another node.
func isOkToMigrate(pv *corev1.PersistentVolume) bool {
	// Unbound pv could relate to a statically created PV, leftover from something else, etc.
	if pv.Status.Phase != corev1.VolumeBound {
		util.Log.Debug("skipping migration, PV not bound", "pv", pv.Name, "phase", pv.Status.Phase)
		return false
	}
	// Volume bound but not associated with claim (perhaps because of order of bind / updatedate pv)
	if pv.Spec.ClaimRef == nil {
		util.Log.Debug("skipping migration, PV has no claimRef", "pv", pv.Name)
		return false
	}
	// For example if the policy is Delete, maybe no point to migrate it when pod will be deleted.
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		util.Log.Debug("skipping migration, PV reclaim policy is not Retain", "pv", pv.Name, "policy", pv.Spec.PersistentVolumeReclaimPolicy)
		return false
	}
	return true
}

// podsToBounce returns the pods among pods that are appropriate to delete.
func podsToBounce(pods []corev1.Pod, claimName, thisNode string) []corev1.Pod {
	var out []corev1.Pod
	for _, pod := range pods {
		// Exclude pods already terminating.
		if pod.DeletionTimestamp != nil {
			continue
		}
		// Exclude pods unscheduled or already scheduled to this node.
		if pod.Spec.NodeName == "" || pod.Spec.NodeName == thisNode {
			continue
		}
		// Exclude pods unrelated to this claim.
		if !podMountsClaim(&pod, claimName) {
			continue
		}
		out = append(out, pod)
	}
	return out
}

// podMountsClaim checks if the pod mounts the given persistenv volume claim.
func podMountsClaim(pod *corev1.Pod, claimName string) bool {
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == claimName {
			return true
		}
	}
	return false
}

// pinNodeAffinity ANDs a kubernetes.io/hostname requirement to pv's first
// NodeSelectorTerm. Any pre-existing hostname requirement (from an earlier migration to a
// different node) is replaced.
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
