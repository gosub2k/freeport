package manager

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// wantRecreatedPV computes what recreatePVForNode should produce from
// original: everything unchanged (same Name, ClaimRef, CSI source, reclaim
// policy, capacity, ...) except the metadata fields that MUST reset on any
// object recreation (ResourceVersion, UID, CreationTimestamp, ManagedFields,
// Generation, Finalizers, Status) and the NodeAffinity mutation that is the
// entire point of migration.
func wantRecreatedPV(original *corev1.PersistentVolume, nodeName string) *corev1.PersistentVolume {
	want := original.DeepCopy()
	want.ResourceVersion = ""
	want.UID = ""
	want.CreationTimestamp = metav1.Time{}
	want.ManagedFields = nil
	want.Finalizers = nil
	want.Generation = 0
	want.Status = corev1.PersistentVolumeStatus{}
	pinNodeAffinity(want, nodeName)
	return want
}

// stripVolatileMetadata clears fields the fake (and real) API server assigns
// on Create that this test has no expected exact value for.
func stripVolatileMetadata(pv *corev1.PersistentVolume) *corev1.PersistentVolume {
	got := pv.DeepCopy()
	got.ResourceVersion = ""
	return got
}

func boundRetainPV(name, volumeHandle, driver, claimNamespace, claimName string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{pvProtectionFinalizer}},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource:        corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: driver, VolumeHandle: volumeHandle}},
			ClaimRef:                      &corev1.ObjectReference{Namespace: claimNamespace, Name: claimName},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			NodeAffinity: &corev1.VolumeNodeAffinity{
				Required: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{Key: "freeport.local/sandisk-cruzer", Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}},
						},
					}},
				},
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
}

func podWithClaim(namespace, name, nodeName, claimName string, owned bool) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Volumes:  []corev1.Volume{{VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName}}}},
		},
	}
	if owned {
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "rs-1", UID: "u1"}}
	}
	return p
}

func TestEligiblePV(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*corev1.PersistentVolume)
		want bool
	}{
		{"bound+claimRef+retain is eligible", func(pv *corev1.PersistentVolume) {}, true},
		{"not bound is ineligible", func(pv *corev1.PersistentVolume) { pv.Status.Phase = corev1.VolumeAvailable }, false},
		{"no claimRef is ineligible", func(pv *corev1.PersistentVolume) { pv.Spec.ClaimRef = nil }, false},
		{"delete reclaim policy is ineligible", func(pv *corev1.PersistentVolume) {
			pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pv := boundRetainPV("pv-1", "vol-1", "freeport.local", "default", "claim-1")
			tt.mut(pv)
			if got := eligiblePV(pv); got != tt.want {
				t.Errorf("eligiblePV = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodsToBounce(t *testing.T) {
	terminating := podWithClaim("default", "terminating", "node-a", "claim-1", true)
	now := metav1.Now()
	terminating.DeletionTimestamp = &now

	pods := []corev1.Pod{
		podWithClaim("default", "already-here", "node-b", "claim-1", true),
		podWithClaim("default", "pending", "", "claim-1", true),
		terminating,
		podWithClaim("default", "elsewhere", "node-a", "claim-1", true),
		podWithClaim("default", "different-claim", "node-a", "claim-2", true),
	}

	got := podsToBounce(pods, "claim-1", "node-b")
	if len(got) != 1 || got[0].Name != "elsewhere" {
		t.Errorf("podsToBounce = %v, want exactly [elsewhere]", got)
	}
}

func TestBouncePod_bareIsSkipped(t *testing.T) {
	pod := podWithClaim("default", "bare", "node-a", "claim-1", false)
	clientset := fake.NewSimpleClientset(&pod)
	m := &Manager{clientset: clientset, nodeName: "node-b", driverName: "freeport.local"}

	if err := m.bouncePod(context.Background(), &pod); err != nil {
		t.Fatalf("bouncePod = %v, want nil error", err)
	}
	if _, err := clientset.CoreV1().Pods("default").Get(context.Background(), "bare", metav1.GetOptions{}); err != nil {
		t.Errorf("bare pod should not have been deleted, but Get failed: %v", err)
	}
}

func TestBouncePod_ownedIsDeleted(t *testing.T) {
	pod := podWithClaim("default", "owned", "node-a", "claim-1", true)
	clientset := fake.NewSimpleClientset(&pod)
	m := &Manager{clientset: clientset, nodeName: "node-b", driverName: "freeport.local"}

	if err := m.bouncePod(context.Background(), &pod); err != nil {
		t.Fatalf("bouncePod = %v, want nil error", err)
	}
	if _, err := clientset.CoreV1().Pods("default").Get(context.Background(), "owned", metav1.GetOptions{}); err == nil {
		t.Error("owned pod should have been deleted, but Get succeeded")
	}
}

func TestPinNodeAffinity_appendsAlongsideExisting(t *testing.T) {
	pv := boundRetainPV("pv-1", "vol-1", "freeport.local", "default", "claim-1")
	pinNodeAffinity(pv, "node-b")

	exprs := pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions
	if len(exprs) != 2 {
		t.Fatalf("MatchExpressions = %v, want 2 entries (device class + hostname)", exprs)
	}
	foundClass, foundHost := false, false
	for _, e := range exprs {
		if e.Key == "freeport.local/sandisk-cruzer" {
			foundClass = true
		}
		if e.Key == hostnameTopologyKey && len(e.Values) == 1 && e.Values[0] == "node-b" {
			foundHost = true
		}
	}
	if !foundClass || !foundHost {
		t.Errorf("MatchExpressions = %v, want both device-class and hostname=node-b", exprs)
	}
}

func TestPinNodeAffinity_replacesPriorHostname(t *testing.T) {
	pv := boundRetainPV("pv-1", "vol-1", "freeport.local", "default", "claim-1")
	pinNodeAffinity(pv, "node-b") // first migration
	pinNodeAffinity(pv, "node-c") // second migration (re-hop)

	exprs := pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions
	if len(exprs) != 2 {
		t.Fatalf("MatchExpressions = %v, want 2 entries after re-migration, not an accumulating duplicate", exprs)
	}
	for _, e := range exprs {
		if e.Key == hostnameTopologyKey && (len(e.Values) != 1 || e.Values[0] != "node-c") {
			t.Errorf("hostname expression = %v, want single value node-c", e)
		}
	}
}

func TestMigrateStaleVolumes_happyPath(t *testing.T) {
	pv := boundRetainPV("pv-1", "vol-1", "freeport.local", "default", "claim-1")
	pod := podWithClaim("default", "app-0", "node-a", "claim-1", true)
	clientset := fake.NewSimpleClientset(pv, &pod)
	m := &Manager{clientset: clientset, nodeName: "node-b", driverName: "freeport.local"}

	dev := mountedDevice{}
	dev.mountpoint = "/mnt/dev1"

	pvsByHandle, err := m.listPVsByVolumeHandle(context.Background())
	if err != nil {
		t.Fatalf("listPVsByVolumeHandle = %v", err)
	}
	if _, ok := pvsByHandle["vol-1"]; !ok {
		t.Fatalf("expected vol-1 to be indexed")
	}

	// Exercise the full per-volume path directly (equivalent to what
	// migrateStaleVolumes does once it has resolved dirName -> pv), since
	// migrateStaleVolumes itself additionally requires real files on disk
	// for volumeDirs() which is exercised separately in TestVolumeDirs.
	newPV, err := m.recreatePVForNode(context.Background(), pv)
	if err != nil {
		t.Fatalf("recreatePVForNode = %v", err)
	}
	want := wantRecreatedPV(pv, "node-b")
	if diff := stripVolatileMetadata(newPV); !reflect.DeepEqual(diff, want) {
		t.Errorf("recreated PV diverges from original beyond the intended changes:\ngot:  %+v\nwant: %+v", diff, want)
	}

	pods, err := m.listPods(context.Background(), "default")
	if err != nil {
		t.Fatalf("listPods = %v", err)
	}
	toBounce := podsToBounce(pods, "claim-1", m.nodeName)
	if len(toBounce) != 1 {
		t.Fatalf("podsToBounce = %v, want exactly 1", toBounce)
	}
	if err := m.bouncePod(context.Background(), &toBounce[0]); err != nil {
		t.Fatalf("bouncePod = %v", err)
	}
	if _, err := clientset.CoreV1().Pods("default").Get(context.Background(), "app-0", metav1.GetOptions{}); err == nil {
		t.Error("bounced pod should have been deleted")
	}
}

func TestRecreatePVForNode_matchesOriginalExceptIntendedChanges(t *testing.T) {
	original := boundRetainPV("pv-1", "vol-1", "freeport.local", "default", "claim-1")
	original.Spec.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}
	original.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	volumeMode := corev1.PersistentVolumeFilesystem
	original.Spec.VolumeMode = &volumeMode
	original.Spec.StorageClassName = "freeport-sandisk"
	clientset := fake.NewSimpleClientset(original)
	m := &Manager{clientset: clientset, nodeName: "node-b", driverName: "freeport.local"}

	got, err := m.recreatePVForNode(context.Background(), original)
	if err != nil {
		t.Fatalf("recreatePVForNode = %v", err)
	}

	want := wantRecreatedPV(original, "node-b")
	if diff := stripVolatileMetadata(got); !reflect.DeepEqual(diff, want) {
		t.Errorf("recreated PV diverges from original beyond the intended changes:\ngot:  %+v\nwant: %+v", diff, want)
	}
}

func TestListPVsByVolumeHandle_ignoresOtherDrivers(t *testing.T) {
	pv := boundRetainPV("pv-1", "vol-1", "other.driver", "default", "claim-1")
	clientset := fake.NewSimpleClientset(pv)
	m := &Manager{clientset: clientset, nodeName: "node-b", driverName: "freeport.local"}

	byHandle, err := m.listPVsByVolumeHandle(context.Background())
	if err != nil {
		t.Fatalf("listPVsByVolumeHandle = %v", err)
	}
	if len(byHandle) != 0 {
		t.Errorf("byHandle = %v, want empty (PV belongs to a different driver)", byHandle)
	}
}
