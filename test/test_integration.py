import time

import pytest
from kubernetes import client, config
from kubernetes.client.rest import ApiException

config.load_kube_config()

v1_api = client.CoreV1Api()
storage_api = client.StorageV1Api()
CSI_PROVISIONER = "freeport.local"


class TestCSIProvisioning:
    @pytest.fixture
    def namespace(self):
        ns_name = "test"
        namespace = client.V1Namespace(metadata=client.V1ObjectMeta(name=ns_name))
        try:
            v1_api.create_namespace(namespace)
        except ApiException as e:
            if e.status != 409:  # ignore AlreadyExists
                raise
        yield ns_name
        v1_api.delete_namespace(ns_name)

    @pytest.fixture
    def storage_class(self):
        yield "freeport-generic"

    def test_pvc_triggers_createvolume_and_binds_pv(self, namespace, storage_class):
        pvc_name = f"test-pvc-{int(time.time())}"
        pod_name = f"test-pod-{int(time.time())}"
        pv_name = None

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
            print(f"✓ PVC {pvc_name} created, waiting for first consumer...")

            # Step 2: Create a pod that consumes the PVC — this is what triggers
            # CreateVolume with WaitForFirstConsumer
            pod = client.V1Pod(
                metadata=client.V1ObjectMeta(name=pod_name, namespace=namespace),
                spec=client.V1PodSpec(
                    restart_policy="Never",
                    containers=[
                        client.V1Container(
                            name="test",
                            image="busybox",
                            command=["sleep", "3600"],
                            volume_mounts=[
                                client.V1VolumeMount(
                                    name="test-vol",
                                    mount_path="/mnt/test",
                                )
                            ],
                        )
                    ],
                    volumes=[
                        client.V1Volume(
                            name="test-vol",
                            persistent_volume_claim=client.V1PersistentVolumeClaimVolumeSource(
                                claim_name=pvc_name
                            ),
                        )
                    ],
                ),
            )
            v1_api.create_namespaced_pod(namespace, pod)
            print(f"✓ Pod {pod_name} created, scheduler should now trigger CreateVolume...")

            # Step 3: Wait for PVC to be bound (CreateVolume fires once pod is scheduled)
            timeout = 60
            start_time = time.time()
            while time.time() - start_time < timeout:
                pvc_status = v1_api.read_namespaced_persistent_volume_claim_status(
                    pvc_name, namespace
                )
                phase = pvc_status.status.phase
                if phase == "Bound":
                    pv_name = pvc_status.spec.volume_name
                    break
                print(f"Waiting for PVC to bind. Status: {phase}")
                time.sleep(2)

            assert pv_name is not None, f"PVC not bound within {timeout}s. Check CSI controller logs."

            # Step 4: Verify PV
            pv = v1_api.read_persistent_volume(pv_name)
            assert pv.status.phase == "Bound", f"PV not Bound: {pv.status.phase}"
            assert pv.spec.claim_ref.name == pvc_name
            assert pv.spec.claim_ref.namespace == namespace

            # Step 5: Verify CSI fields
            assert pv.spec.csi is not None, "PV is not a CSI volume"
            assert pv.spec.csi.driver == CSI_PROVISIONER, f"Wrong driver: {pv.spec.csi.driver}"
            assert pv.spec.csi.volume_handle is not None, "No volumeHandle — CreateVolume didn't return an ID"

            # Step 6: Verify provisioned-by annotation
            assert pv.metadata.annotations.get("pv.kubernetes.io/provisioned-by") == CSI_PROVISIONER

            print(f"✓ PV {pv_name} created and bound")
            print(f"  VolumeHandle: {pv.spec.csi.volume_handle}")
            print(f"  Capacity: {pv.spec.capacity['storage']}")

        finally:
            self._cleanup_pod(pod_name, namespace)
            self._cleanup_pvc_pv(pvc_name, pv_name, namespace)

    def _cleanup_pod(self, pod_name, namespace):
        try:
            v1_api.delete_namespaced_pod(pod_name, namespace, grace_period_seconds=0)
            self._wait_for_resource_deleted(
                lambda: v1_api.read_namespaced_pod(pod_name, namespace),
                resource_name=f"Pod/{pod_name}",
                timeout=30,
            )
        except ApiException as e:
            if e.status != 404:
                raise

    def _cleanup_pvc_pv(self, pvc_name, pv_name, namespace):
        if pvc_name:
            try:
                v1_api.delete_namespaced_persistent_volume_claim(
                    pvc_name, namespace, grace_period_seconds=0
                )
                self._wait_for_resource_deleted(
                    lambda: v1_api.read_namespaced_persistent_volume_claim(pvc_name, namespace),
                    resource_name=f"PVC/{pvc_name}",
                    timeout=30,
                )
            except ApiException as e:
                if e.status == 404:
                    pass
                else:
                    print(f"⚠ PVC {pvc_name} stuck, removing finalizers...")
                    self._remove_finalizers_pvc(pvc_name, namespace)
                    try:
                        v1_api.delete_namespaced_persistent_volume_claim(
                            pvc_name, namespace, grace_period_seconds=0
                        )
                    except ApiException:
                        pass

        if pv_name:
            try:
                pv = v1_api.read_persistent_volume(pv_name)
                if pv.metadata.deletion_timestamp:
                    print(f"⚠ PV {pv_name} stuck in Terminating, removing finalizers...")
                    self._remove_finalizers_pv(pv_name)
                v1_api.delete_persistent_volume(pv_name, grace_period_seconds=0)
                self._wait_for_resource_deleted(
                    lambda: v1_api.read_persistent_volume(pv_name),
                    resource_name=f"PV/{pv_name}",
                    timeout=30,
                )
            except ApiException as e:
                if e.status != 404:
                    raise

    def _remove_finalizers_pvc(self, pvc_name, namespace):
        try:
            v1_api.patch_namespaced_persistent_volume_claim(
                pvc_name, namespace, {"metadata": {"finalizers": None}}
            )
        except ApiException as e:
            if e.status != 404:
                raise

    def _remove_finalizers_pv(self, pv_name):
        try:
            v1_api.patch_persistent_volume(pv_name, {"metadata": {"finalizers": None}})
        except ApiException as e:
            if e.status != 404:
                raise

    def _wait_for_resource_deleted(self, read_func, resource_name, timeout=30):
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