#!/usr/bin/env python3
"""Kubernetes-backed USB device manager.

Same discovery logic as InMemoryUSBDeviceManager (inherited) — only the
*known-device registry* is persisted, as BlockDevice custom resources
(one per serial, cluster-scoped) instead of an in-memory list.

A device gone from the system has its CR deleted: the CR exists iff the
device is currently present in the cluster.

Needs the CRD applied first (deploy/blockdevice-crd.yaml) and RBAC to
get/list/create/patch/delete blockdevices.freeport.local.
"""

import re
import subprocess

from kubernetes import client, config
from kubernetes.client.exceptions import ApiException

from abstract_manager import BlockDeviceType, DeviceFilter, HostBlockDevice, Serial, log
from inmemory_manager import InMemoryUSBDeviceManager

GROUP = "freeport.local"
VERSION = "v1alpha1"
PLURAL = "blockdevices"


def _cr_name(serial: str) -> str:
    """Map a device serial to a DNS-1123-safe CR name. The CR is keyed on
    serial alone, since serials are assumed unique.

        >>> _cr_name("900053B3E984FA19")
        '900053b3e984fa19'
        >>> _cr_name("USB_DISK 2.0:0")
        'usb-disk-2.0-0'
    """
    s = re.sub(r"[^a-z0-9.-]", "-", str(serial).lower()).strip("-.")
    return (s or "unknown")[:253]


def _from_cr(obj: dict) -> HostBlockDevice:
    """Build a HostBlockDevice from a BlockDevice CR's spec.

    >>> dev = _from_cr({"spec": {"manufacturer": "SanDisk", "model": "Ultra",
    ...                          "serial": "ABC123", "type": "usb",
    ...                          "partition": 1}})
    >>> dev.serial, dev.type
    ('ABC123', <BlockDeviceType.USB: 'usb'>)
    """
    s = obj.get("spec", {})
    return HostBlockDevice(
        manufacturer=s.get("manufacturer", ""),
        type=BlockDeviceType(s.get("type", "usb")),
        model=s.get("model", ""),
        serial=Serial(s.get("serial", "")),
        partition=s.get("partition", 0),
        free=s.get("free", 0),
        mountpoint=s.get("mountpoint", ""),
    )


class K8sUSBDeviceManager(InMemoryUSBDeviceManager):
    """Persists the known-device registry as BlockDevice custom resources.

    Inherits all of InMemoryUSBDeviceManager's discovery (by-id scan + sysfs
    attribute reads); overrides only add / get / remove of known devices.
    """

    def __init__(
        self,
        filter: DeviceFilter,
        node_name: str,
        in_cluster: bool = True,
        usb_devdir: str | None = None,
        sys_class_block: str | None = None,
        host_mount: str | None = None,
    ) -> None:
        self._filter = filter
        self._node = node_name
        self._USB_DEVDIR = usb_devdir or self._USB_DEVDIR
        self._SYS_CLASS_BLOCK = sys_class_block or self._SYS_CLASS_BLOCK
        self._HOST_MOUNT = host_mount or self._HOST_MOUNT
        # in_cluster: running as a pod (use the mounted ServiceAccount);
        # otherwise load the local ~/.kube/config (dev / out-of-cluster).
        if in_cluster:
            config.load_incluster_config()
        else:
            config.load_kube_config()
        self._api = client.CustomObjectsApi()

    def replace_dev_path(self, dev_path):
        return re.sub(r"/mnt", self._HOST_MOUNT, dev_path)

    def mount(self, dev_path: str, serial: str) -> str:
        dev_path = self.replace_dev_path(dev_path)

        _NSENTER = ["nsenter", "--target", "1", "--mount", "--"]
        tmp_run = subprocess.run

        def nsenter_run(cmd, *args, **kwargs):
            return tmp_run(_NSENTER + list(cmd), *args, **kwargs)

        subprocess.run = nsenter_run
        ret = super().mount(dev_path, serial)
        subprocess.run = tmp_run
        return ret

    def get_df(self, dev_path: str) -> int:
        dev_path = self.replace_dev_path(dev_path)
        return super().get_df(dev_path)

    # ---- known-device registry (backed by the cluster) ------------------- #

    def _get_known_devices(self) -> list[HostBlockDevice]:
        try:
            resp = self._api.list_cluster_custom_object(
                GROUP,
                VERSION,
                PLURAL,
            )
        except ApiException:
            log.exception("listing BlockDevices failed")
            return []
        return [
            _from_cr(o)
            for o in resp.get("items", [])
            if o.get("spec").get("node") == self._node
        ]

    def _add_device_to_known_devices(self, device: HostBlockDevice) -> bool:
        name = _cr_name(device.serial)

        try:
            existing = self._api.get_cluster_custom_object(GROUP, VERSION, PLURAL, name)
        except ApiException as e:
            if e.status != 404:
                raise
            existing = None

        if existing is not None:
            if existing.get("spec").get("node", "") == self._node:
                return False  # already exists on this node (by serial no SOT)
            else:
                # seems like the device migrated to this node:
                patch = {
                    "spec": {
                        "node": self._node,
                        # "manufacturer": device.manufacturer,
                        # "model": device.model,
                        # "type": device.type.value,
                        # "partition": device.partition,
                    },
                }
            # TODO - make this a CAS operation
            self._api.patch_cluster_custom_object(GROUP, VERSION, PLURAL, name, patch)
            prev_node = existing.get("spec").get("node", "")
            log.info(
                "BlockDevice updated (node %s -> %s): %s", prev_node, self._node, device
            )
            return True

        else:
            body = {
                "apiVersion": f"{GROUP}/{VERSION}",
                "kind": "BlockDevice",
                "metadata": {
                    "name": name,
                },
                "spec": {
                    "node": self._node,
                    "manufacturer": device.manufacturer,
                    "model": device.model,
                    "serial": str(device.serial),
                    "type": device.type.value,
                    "partition": device.partition,
                    "free": device.free,
                },
            }
            self._api.create_cluster_custom_object(GROUP, VERSION, PLURAL, body)
            log.info("BlockDevice created: %s", device)
            return True

    def _remove_device_from_known_devices(self, device: HostBlockDevice) -> bool:
        name = _cr_name(device.serial)
        try:
            existing = self._api.get_cluster_custom_object(GROUP, VERSION, PLURAL, name)
        except ApiException as e:
            if e.status == 404:
                return False
            raise

        # Not necessary because the other functions check owner, but
        # Re-check the owner of the block device explicitly
        if existing.get("spec").get("node") != self._node:
            log.info(
                "not deleting BlockDevice %s: owned by %s, not us (%s)",
                name,
                existing.get("spec").get("node"),
                self._node,
            )
            return False

        # Probably not needed because of slow period of other reconcile loops in k8s, but:
        # Compare and delete atomically - avoid deleting if its owned by another node.
        meta = existing.get("metadata")
        opts = client.V1DeleteOptions(
            preconditions=client.V1Preconditions(
                resource_version=meta.get("resourceVersion"),
                uid=meta.get("uid"),
            )
        )
        try:
            self._api.delete_cluster_custom_object(
                GROUP, VERSION, PLURAL, name, body=opts
            )
        except ApiException as e:
            # 404: already gone; 409: changed under us (e.g. migrated) -> skip.
            if e.status in (404, 409):
                return False
            raise
        log.info("BlockDevice deleted: %s", device)
        return True


if __name__ == "__main__":
    from os import environ
    from time import sleep

    node_name = environ.get("NODE_NAME")
    if node_name is None:
        log.fatal("NODE_NAME not defined")
        exit(1)
    try:
        mngr = K8sUSBDeviceManager(
            filter=DeviceFilter(),
            node_name=str(node_name),
            usb_devdir=environ["USB_DEVDIR"],
            sys_class_block=environ["SYS_CLASS_BLOCK"],
            host_mount=environ["HOST_MOUNT"],
        )
    except config.ConfigException:
        # device manager inside k8s didnt find k8s api
        mngr = K8sUSBDeviceManager(
            filter=DeviceFilter(),
            node_name=str(node_name),
            in_cluster=False,
            usb_devdir=environ["USB_DEVDIR"],
            sys_class_block=environ["SYS_CLASS_BLOCK"],
            host_mount=environ["HOST_MOUNT"],
        )
    except KeyError as e:
        log.fatal(f"must set USB_DEVDIR, SYS_CLASS_BLOCK, HOST_MOUNT: {e}")
        exit(2)
    while True:
        mngr.reconcile()
        sleep(5)
