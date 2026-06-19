import time

import pytest
from kubernetes import client, config
from kubernetes.client.rest import ApiException

# Load kubeconfig (use in-cluster config for CI/CD)
config.load_kube_config()  # or config.load_incluster_config()

v1_api = client.CoreV1Api()
storage_api = client.StorageV1Api()
CSI_PROVISIONER = "freeport.local"

class TestCSIProvisioning:
    @pytest.fixture
    def namespace(self):
        """Create isolated namespace for test"""
        # ns_name = f"test-csi-{int(time.time())}"
        ns_name = "test"
        namespace = client.V1Namespace(metadata=client.V1ObjectMeta(name=ns_name))
        v1_api.create_namespace(namespace)
        yield ns_name
        # Cleanup
        v1_api.delete_namespace(ns_name)

    @pytest.fixture
    def storage_class(self):
        yield "freeport-generic-immediate"

    def test_pvc_triggers_createvolume_and_binds_pv(self, namespace, storage_class):
        """
        Verify that creating a PVC triggers CSI CreateVolume and PV is bound.
        This confirms the external-provisioner sidecar called Controller.CreateVolume.
        """
        pvc_name = f"test-pvc-{int(time.time())}"

        try:
            # Step 1: Create PVC
            pvc = client.V1PersistentVolumeClaim(
                metadata=client.V1ObjectMeta(name=pvc_name, namespace=namespace),
                spec=client.V1PersistentVolumeClaimSpec(
                    access_modes=["ReadWriteOnce"],
                    storage_class_name=storage_class,
                    resources=client.V1ResourceRequirements(
                        requests={"storage": "1Gi"}
                    ),
                ),
            )
            v1_api.create_namespaced_persistent_volume_claim(namespace, pvc)

            # Step 2: Wait for PV to be provisioned and bound (timeout: 60s)
            pv_name = None
            timeout = 60
            start_time = time.time()

            while time.time() - start_time < timeout:
                try:
                    pvc_status = v1_api.read_namespaced_persistent_volume_claim_status(
                        pvc_name, namespace
                    )

                    # Check if PVC is bound
                    status_phase = pvc_status.status.phase
                    if status_phase == "Bound":
                        pv_name = pvc_status.spec.volume_name
                        break
                    print(f"Waiting for pvc to be bound. Status: {status_phase}")
                    time.sleep(1)

                except ApiException as e:
                    if e.status != 404:
                        raise
                time.sleep(2)

            # Assert PV was created
            assert (
                pv_name is not None
            ), f"PVC was not bound within {timeout}s. Check CSI controller logs."

            # Step 3: Verify PV object exists and is bound
            pv = v1_api.read_persistent_volume(pv_name)
            assert (
                pv.status.phase == "Bound"
            ), f"PV {pv_name} is not in Bound state: {pv.status.phase}"
            assert pv.spec.claim_ref is not None, "PV has no claimRef"
            assert pv.spec.claim_ref.name == pvc_name, "PV claimRef doesn't match PVC"
            assert (
                pv.spec.claim_ref.namespace == namespace
            ), "PV claimRef namespace doesn't match"

            # Step 4: Verify PV is CSI volume with correct driver
            assert pv.spec.csi is not None, "PV is not a CSI volume"
            assert (
                pv.spec.csi.driver == CSI_PROVISIONER
            ), f"Wrong CSI driver: {pv.spec.csi.driver}"
            assert (
                pv.spec.csi.volume_handle is not None
            ), "PV has no volumeHandle (CreateVolume didn't return volume ID)"

            # Step 5: Verify annotations indicate dynamic provisioning
            assert (
                "pv.kubernetes.io/provisioned-by" in pv.metadata.annotations
            ), "PV missing 'provisioned-by' annotation"
            assert (
                pv.metadata.annotations["pv.kubernetes.io/provisioned-by"]
                == CSI_PROVISIONER
            )

            # Optional Step 6: Verify volume exists in storage backend
            # (Requires access to your storage API)
            # storage_client = get_storage_backend_client()
            # volume = storage_client.get_volume(pv.spec.csi.volume_handle)
            # assert volume is not None, f"Volume {pv.spec.csi.volume_handle} not found in storage backend"
            # assert volume.size_bytes >= 1024 * 1024 * 1024  # 1Gi

            print(f"✓ PV {pv_name} successfully created and bound")
            print(f"  VolumeHandle: {pv.spec.csi.volume_handle}")
            print(f"  Capacity: {pv.spec.capacity['storage']}")
        finally:
            # Cleanup: Delete PVC first, then PV (with finalizer removal if needed)
            self._cleanup_pvc_pv(pvc_name, pv_name, namespace)

    def _cleanup_pvc_pv(self, pvc_name, pv_name, namespace):
        """
        Safely delete PVC and PV, handling finalizers.
        Order: Delete PVC first → PV will be auto-deleted if reclaimPolicy=Delete
        """
        # Step 1: Delete PVC first (triggers PV deletion if reclaimPolicy=Delete)
        if pvc_name:
            try:
                v1_api.delete_namespaced_persistent_volume_claim(
                    pvc_name, namespace, grace_period_seconds=0
                )
                # Wait for PVC to be fully deleted
                self._wait_for_resource_deleted(
                    lambda: v1_api.read_namespaced_persistent_volume_claim(
                        pvc_name, namespace
                    ),
                    resource_name=f"PVC/{pvc_name}",
                    timeout=30,
                )
            except ApiException as e:
                if e.status == 404:
                    pass  # Already deleted
                else:
                    # PVC stuck - remove finalizer and force delete
                    print(f"⚠ PVC {pvc_name} stuck, removing finalizers...")
                    self._remove_finalizers_pvc(pvc_name, namespace)
                    try:
                        v1_api.delete_namespaced_persistent_volume_claim(
                            pvc_name, namespace, grace_period_seconds=0
                        )
                    except ApiException:
                        pass

        # Step 2: Delete PV if it still exists (for reclaimPolicy=Retain or stuck PVs)
        if pv_name:
            try:
                # Check if PV still exists
                pv = v1_api.read_persistent_volume(pv_name)

                # If PV is stuck in Terminating, remove finalizers
                if pv.metadata.deletion_timestamp:
                    print(
                        f"⚠ PV {pv_name} stuck in Terminating, removing finalizers..."
                    )
                    self._remove_finalizers_pv(pv_name)

                # Delete PV
                v1_api.delete_persistent_volume(pv_name, grace_period_seconds=0)

                # Wait for PV deletion
                self._wait_for_resource_deleted(
                    lambda: v1_api.read_persistent_volume(pv_name),
                    resource_name=f"PV/{pv_name}",
                    timeout=30,
                )

            except ApiException as e:
                if e.status == 404:
                    pass  # Already deleted
                else:
                    raise

    def _remove_finalizers_pvc(self, pvc_name, namespace):
        """Remove all finalizers from PVC to force deletion"""
        patch_body = {"metadata": {"finalizers": None}}
        try:
            v1_api.patch_namespaced_persistent_volume_claim(
                pvc_name, namespace, patch_body
            )
        except ApiException as e:
            if e.status != 404:
                raise

    def _remove_finalizers_pv(self, pv_name):
        """Remove all finalizers from PV to force deletion"""
        patch_body = {"metadata": {"finalizers": None}}
        try:
            v1_api.patch_persistent_volume(pv_name, patch_body)
        except ApiException as e:
            if e.status != 404:
                raise

    def _wait_for_resource_deleted(self, read_func, resource_name, timeout=30):
        """Wait for resource to be fully deleted"""
        start_time = time.time()
        while time.time() - start_time < timeout:
            try:
                read_func()
                time.sleep(1)
            except ApiException as e:
                if e.status == 404:
                    print(f"✓ {resource_name} deleted")
                    return
                raise
        pytest.fail(f"{resource_name} not deleted within {timeout}s")
