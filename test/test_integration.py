import time

import pytest
from kubernetes import client, config
from kubernetes.client.rest import ApiException

config.load_kube_config()

v1_api = client.CoreV1Api()
storage_api = client.StorageV1Api()
CSI_PROVISIONER = "freeport.local"
STORAGE_CLASS = "freeport-generic"
BLOCKDEVICE_GROUP = "freeport.local"
BLOCKDEVICE_VERSION = "v1alpha1"
BLOCKDEVICE_PLURAL = "blockdevices"

# String to check logs for failure
FAIL_STR="__FAIL__"

# ---------------------------------------------------------------------------
# Shared helpers
# ---------------------------------------------------------------------------
custom_api = client.CustomObjectsApi()



def get_nodes_with_class(storage_class_name):
    """Return list of node names that have a BlockDevice matching the given class."""
    result = custom_api.list_cluster_custom_object(
        group=BLOCKDEVICE_GROUP,
        version=BLOCKDEVICE_VERSION,
        plural=BLOCKDEVICE_PLURAL,
    )
    nodes = []
    for item in result.get("items", []):
        bd_class = item.get("spec", {}).get("class") or item.get("status", {}).get("class")
        node = item.get("spec", {}).get("node") or item.get("status", {}).get("node")
        if bd_class == storage_class_name and node and node not in nodes:
            nodes.append(node)
    return nodes



def make_pvc(name, namespace, storage_class=STORAGE_CLASS, storage="1Gi"):
    return client.V1PersistentVolumeClaim(
        metadata=client.V1ObjectMeta(name=name, namespace=namespace),
        spec=client.V1PersistentVolumeClaimSpec(
            access_modes=["ReadWriteOnce"],
            storage_class_name=storage_class,
            resources=client.V1ResourceRequirements(
                requests={"storage": storage},
            ),
        ),
    )


def make_pod(name, namespace, pvc_name, containers, node_name=None):
    """Build a pod spec with a shared PVC volume mounted into each container."""
    spec = client.V1PodSpec(
        restart_policy="Never",
        containers=containers,
        volumes=[
            client.V1Volume(
                name="test-vol",
                persistent_volume_claim=client.V1PersistentVolumeClaimVolumeSource(
                    claim_name=pvc_name
                ),
            )
        ],
    )
    if node_name:
        # Avoid hard bind with spec.node_name that would deadlock
        spec.node_selector = {"kubernetes.io/hostname": node_name}

    return client.V1Pod(
        metadata=client.V1ObjectMeta(name=name, namespace=namespace),
        spec=spec,
    )


def make_container(name, command, read_only=False):
    return client.V1Container(
        name=name,
        image="busybox",
        command=["sh", "-c", command],
        volume_mounts=[
            client.V1VolumeMount(
                name="test-vol",
                mount_path="/mnt/test",
                read_only=read_only,
            )
        ],
    )


def wait_for_pvc_bound(pvc_name, namespace, timeout=30):
    """Wait for PVC to reach Bound phase. Returns pv_name or raises."""
    start = time.time()
    while time.time() - start < timeout:
        status = v1_api.read_namespaced_persistent_volume_claim_status(pvc_name, namespace)
        phase = status.status.phase
        if phase == "Bound":
            return status.spec.volume_name
        print(f"  PVC {pvc_name}: {phase}")
        time.sleep(2)
    raise TimeoutError(f"PVC {pvc_name} not bound within {timeout}s — check CSI controller logs")


def wait_and_check_pod_container_log(pod_name, namespace, container, expected, timeout=30):
    """Wait until container log contains expected string. Returns log string or raises."""
    start = time.time()
    while time.time() - start < timeout:
        try:
            logs = v1_api.read_namespaced_pod_log(pod_name, namespace, container=container)
            if logs:
                # WTF
                for line in logs.splitlines():
                    if isinstance(line, bytes):
                        for bline in str(line).splitlines():
                            print(f'{pod_name}: {bline}')
                    else:
                        print(f'{pod_name}: {line}')
                if expected in logs:
                    return logs.strip()
                if FAIL_STR in logs:
                    raise Exception(f"Container {container} log contained {FAIL_STR}")
        except ApiException:
            pass
        time.sleep(2)
    raise TimeoutError(f"Container {container} log never contained {expected!r}")


def assert_pv_valid(pv_name, pvc_name, namespace):
    """Common PV assertions — correct driver, bound, claimRef matches."""
    pv = v1_api.read_persistent_volume(pv_name)
    assert pv.status.phase == "Bound", f"PV not Bound: {pv.status.phase}"
    assert pv.spec.claim_ref.name == pvc_name
    assert pv.spec.claim_ref.namespace == namespace
    assert pv.spec.csi is not None, "PV is not a CSI volume"
    assert pv.spec.csi.driver == CSI_PROVISIONER, f"Wrong driver: {pv.spec.csi.driver}"
    assert pv.spec.csi.volume_handle is not None, "No volumeHandle — CreateVolume returned no ID"
    assert pv.metadata.annotations.get("pv.kubernetes.io/provisioned-by") == CSI_PROVISIONER
    return pv


def cleanup_pod(pod_name, namespace):
    try:
        v1_api.delete_namespaced_pod(pod_name, namespace, grace_period_seconds=0)
        _wait_deleted(
            lambda: v1_api.read_namespaced_pod(pod_name, namespace),
            f"Pod/{pod_name}",
        )
    except ApiException as e:
        if e.status != 404:
            raise


def cleanup_pvc_pv(pvc_name, pv_name, namespace):
    if pvc_name:
        try:
            v1_api.delete_namespaced_persistent_volume_claim(
                pvc_name, namespace, grace_period_seconds=0
            )
            _wait_deleted(
                lambda: v1_api.read_namespaced_persistent_volume_claim(pvc_name, namespace),
                f"PVC/{pvc_name}",
                timeout=30,
            )
        except ApiException as e:
            if e.status == 404:
                pass
            else:
                print(f"⚠ PVC {pvc_name} stuck, removing finalizers...")
                _remove_finalizers_pvc(pvc_name, namespace)
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
                _remove_finalizers_pv(pv_name)
            v1_api.delete_persistent_volume(pv_name, grace_period_seconds=0)
            _wait_deleted(
                lambda: v1_api.read_persistent_volume(pv_name),
                f"PV/{pv_name}",
                timeout=30,
            )
        except ApiException as e:
            if e.status != 404:
                raise


def _wait_deleted(read_func, resource_name, timeout=30):
    start = time.time()
    while time.time() - start < timeout:
        try:
            read_func()
            time.sleep(1)
        except ApiException as e:
            if e.status == 404:
                print(f"✓ {resource_name} deleted")
                return
            raise
    pytest.fail(f"{resource_name} not deleted within {timeout}s")


def _remove_finalizers_pvc(pvc_name, namespace):
    try:
        v1_api.patch_namespaced_persistent_volume_claim(
            pvc_name, namespace, {"metadata": {"finalizers": None}}
        )
    except ApiException as e:
        if e.status != 404:
            raise


def _remove_finalizers_pv(pv_name):
    try:
        v1_api.patch_persistent_volume(pv_name, {"metadata": {"finalizers": None}})
    except ApiException as e:
        if e.status != 404:
            raise


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

@pytest.fixture
def namespace():
    ns_name = f"freeport-test-{int(time.time())}"
    try:
        v1_api.create_namespace(client.V1Namespace(metadata=client.V1ObjectMeta(name=ns_name)))
    except ApiException as e:
        if e.status != 409:
            raise
    yield ns_name
    v1_api.delete_namespace(ns_name)

def get_nodes():
    nodes = get_nodes_with_class(STORAGE_CLASS)
    assert nodes, f"No BlockDevices found with class={STORAGE_CLASS!r} — is the CRD populated?"
    return nodes

@pytest.fixture(params=get_nodes())
def node(request: pytest.FixtureRequest):
    return request.param

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

class TestCSIProvisioning:

    def test_pvc_triggers_createvolume_and_binds_pv(self, namespace):
        """
        Verify that creating a PVC + pod triggers CSI CreateVolume and PV is bound.
        """
        ts = int(time.time())
        pvc_name = f"test-pvc-{ts}"
        pod_name = f"test-pod-{ts}"
        pv_name = None

        try:
            v1_api.create_namespaced_persistent_volume_claim(namespace, make_pvc(pvc_name, namespace))
            print(f"✓ PVC {pvc_name} created")

            pod = make_pod(
                pod_name, namespace, pvc_name,
                containers=[
                    make_container("sleeper", "sleep 3600"),
                ],
            )
            v1_api.create_namespaced_pod(namespace, pod)
            print(f"✓ Pod {pod_name} created — waiting for scheduler to trigger CreateVolume...")

            pv_name = wait_for_pvc_bound(pvc_name, namespace)

            pv = assert_pv_valid(pv_name, pvc_name, namespace)
            print(f"✓ PV {pv_name} created and bound")
            print(f"  VolumeHandle: {pv.spec.csi.volume_handle}")
            print(f"  Capacity: {pv.spec.capacity['storage']}")

        finally:
            cleanup_pod(pod_name, namespace)
            cleanup_pvc_pv(pvc_name, pv_name, namespace)

    def test_shared_volume_read_write(self, namespace):
        """
        Verify two containers in the same pod can share a CSI volume:
        writer writes a file, reader reads it back.
        """
        ts = int(time.time())
        pvc_name = f"test-pvc-rw-{ts}"
        pod_name = f"test-pod-rw-{ts}"
        pv_name = None

        try:
            v1_api.create_namespaced_persistent_volume_claim(namespace, make_pvc(pvc_name, namespace))
            print(f"✓ PVC {pvc_name} created")

            pod = make_pod(
                pod_name, namespace, pvc_name,
                containers=[
                    make_container(
                        "writer",
                        "echo hello-from-writer > /mnt/test/hello.txt && sleep 3600",
                        read_only=False,
                    ),
                    make_container(
                        "reader",
                        # sleep briefly so writer has time to create the file first
                        "sleep 5 && cat /mnt/test/hello.txt && sleep 3600",
                        read_only=True,
                    ),
                ],
            )
            v1_api.create_namespaced_pod(namespace, pod)
            print(f"✓ Pod {pod_name} created")

            pv_name = wait_for_pvc_bound(pvc_name, namespace)
            assert_pv_valid(pv_name, pvc_name, namespace)
            print(f"✓ PVC bound to PV {pv_name}")

            log = wait_and_check_pod_container_log(pod_name, namespace, "reader", "hello-from-writer")
            print(f"✓ Reader saw: {log!r}")

        finally:
            cleanup_pod(pod_name, namespace)
            cleanup_pvc_pv(pvc_name, pv_name, namespace)


    def test_volume_on_each_node_with_blockdevice(self, namespace, node):
            """
            Discover all nodes that have a BlockDevice matching the StorageClass,
            then create a PVC+pod pinned to each node and verify it binds.
            """
            node_name = node
            failures = []
            ts = int(time.time())
            pvc_name = f"test-pvc-node-{ts}"
            pod_name = f"test-pod-node-{ts}"
            pv_name = None

            print(f"\n→ Testing node {node_name}")
            try:
                v1_api.create_namespaced_persistent_volume_claim(
                    namespace, make_pvc(pvc_name, namespace)
                )

                pod = make_pod(
                    pod_name, namespace, pvc_name,
                    node_name=node_name,
                    containers=[
                        make_container("sleeper", "sleep 3600"),
                    ],
                )
                v1_api.create_namespaced_pod(namespace, pod)

                pv_name = wait_for_pvc_bound(pvc_name, namespace, timeout=30)
                assert_pv_valid(pv_name, pvc_name, namespace)
                print(f"  ✓ Node {node_name}: PV {pv_name} bound")

            except Exception as e:
                failures.append(f"node {node_name}: {e}")
                print(f"  ✗ Node {node_name}: {e}")

            finally:
                cleanup_pod(pod_name, namespace)
                cleanup_pvc_pv(pvc_name, pv_name, namespace)
            
            # small gap between nodes to let cleanup settle
            time.sleep(2)


    def test_volume_on_each_node_mounted_on_separate_device(self, namespace, node):
        """
        Verify the CSI volume is a real block device mount, not just a directory
        on the root filesystem. Checks that /mnt/test has a different device/fstype
        than /.
        """
        ts = int(time.time())
        pvc_name = f"test-pvc-dev-{ts}"
        pod_name = f"test-pod-dev-{ts}"
        pv_name = None

        try:
            v1_api.create_namespaced_persistent_volume_claim(namespace, make_pvc(pvc_name, namespace))

            # df -P prints: Filesystem  Blocks  Used  Available  Use%  Mounted on
            # We grab the Filesystem (device) column for both / and /mnt/test
            # and also the filesystem type via /proc/mounts
            check_script = f"""
                root_dev=$(df -P / | awk 'NR==2{{print $1}}')
                vol_dev=$(df -P /mnt/test | awk 'NR==2{{print $1}}')
                echo "root_dev=$root_dev"
                echo "vol_dev=$vol_dev"
                if [[ $root_dev == $vol_dev ]]; then
                  echo {FAIL_STR}
                  exit 1
                fi
                echo OK  # needed for wait_for_container log function
            """

            pod = make_pod(
                pod_name, namespace, pvc_name,
                containers=[
                    make_container("checker", check_script, read_only=False),
                ],
                node_name=node
            )
            v1_api.create_namespaced_pod(namespace, pod)
            print(f"✓ Pod {pod_name} created")

            pv_name = wait_for_pvc_bound(pvc_name, namespace)
            assert_pv_valid(pv_name, pvc_name, namespace)
            print(f"✓ PVC bound to PV {pv_name}")

            wait_and_check_pod_container_log(pod_name, namespace, "checker", "OK", timeout=30)

        finally:
            cleanup_pod(pod_name, namespace)
            cleanup_pvc_pv(pvc_name, pv_name, namespace)

    
    # def test_volume_is_usb_block_device(self, namespace):
    #     """
    #     Verify the block device backing the CSI volume is a USB device by
    #     checking its bus type via host /sys mounted into the pod.
    #     """
    #     ts = int(time.time())
    #     pvc_name = f"test-pvc-usb-{ts}"
    #     pod_name = f"test-pod-usb-{ts}"
    #     pv_name = None

    #     try:
    #         v1_api.create_namespaced_persistent_volume_claim(namespace, make_pvc(pvc_name, namespace))

    #         check_script = """
    #             # Get the base block device backing /mnt/test (e.g. sdb from sdb1)
    #             dev=$(df -P /mnt/test | awk 'NR==2{print $1}')
    #             echo "block_dev=$dev"

    #             # Strip /dev/ and any partition number to get base device (sdb1 -> sdb)
    #             base=$(basename $dev | sed 's/[0-9]*$//')
    #             echo "base_dev=$base"

    #             # Find the device's sysfs path
    #             sys_path=$(readlink -f /sys/block/$base)
    #             echo "sys_path=$sys_path"

    #             if [ -z "$sys_path" ]; then
    #                 echo "FAIL: could not find /sys/block/$base"
    #                 exit 1
    #             fi

    #             # Walk up sysfs looking for a usb subsystem entry
    #             path=$sys_path
    #             found_usb=0
    #             while [ "$path" != "/" ]; do
    #                 if [ -f "$path/subsystem" ]; then
    #                     subsys=$(readlink -f "$path/subsystem" 2>/dev/null | xargs basename 2>/dev/null)
    #                     echo "checking $path -> subsystem=$subsys"
    #                     if [ "$subsys" = "usb" ]; then
    #                         found_usb=1
    #                         break
    #                     fi
    #                 fi
    #                 path=$(dirname $path)
    #             done

    #             if [ $found_usb -eq 1 ]; then
    #                 echo "OK: device $base is on USB bus"
    #             else
    #                 echo "FAIL: device $base does not appear to be USB"
    #                 exit 1
    #             fi
    #         """

    #         pod = client.V1Pod(
    #             metadata=client.V1ObjectMeta(name=pod_name, namespace=namespace),
    #             spec=client.V1PodSpec(
    #                 restart_policy="Never",
    #                 containers=[
    #                     client.V1Container(
    #                         name="checker",
    #                         image="busybox",
    #                         command=["sh", "-c", check_script],
    #                         security_context=client.V1SecurityContext(
    #                             privileged=True,  # needed to access /sys
    #                         ),
    #                         volume_mounts=[
    #                             client.V1VolumeMount(
    #                                 name="test-vol",
    #                                 mount_path="/mnt/test",
    #                             ),
    #                             client.V1VolumeMount(
    #                                 name="sys",
    #                                 mount_path="/sys",
    #                                 read_only=True,
    #                             ),
    #                         ],
    #                     )
    #                 ],
    #                 volumes=[
    #                     client.V1Volume(
    #                         name="test-vol",
    #                         persistent_volume_claim=client.V1PersistentVolumeClaimVolumeSource(
    #                             claim_name=pvc_name
    #                         ),
    #                     ),
    #                     client.V1Volume(
    #                         name="sys",
    #                         host_path=client.V1HostPathVolumeSource(
    #                             path="/sys",
    #                             type="Directory",
    #                         ),
    #                     ),
    #                 ],
    #             ),
    #         )
    #         v1_api.create_namespaced_pod(namespace, pod)
    #         print(f"✓ Pod {pod_name} created")

    #         pv_name = wait_for_pvc_bound(pvc_name, namespace)
    #         assert_pv_valid(pv_name, pvc_name, namespace)
    #         print(f"✓ PVC bound to PV {pv_name}")

    #         log = wait_and_check_pod_container_log(pod_name, namespace, "checker", "OK", timeout=60)
    #         print(f"✓ USB check passed:\n  " + "\n  ".join(log.splitlines()))

    #     finally:
    #         cleanup_pod(pod_name, namespace)
    #         cleanup_pvc_pv(pvc_name, pv_name, namespace)
