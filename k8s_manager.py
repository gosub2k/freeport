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

from kubernetes import client, config
from kubernetes.client.exceptions import ApiException

from provisioner import (
    BlockDeviceType,
    DeviceFilter,
    HostBlockDevice,
    InMemoryUSBDeviceManager,
    Serial,
    log,
)

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
    )


class K8sUSBDeviceManager(InMemoryUSBDeviceManager):
    """Persists the known-device registry as BlockDevice custom resources.

    Inherits all of InMemoryUSBDeviceManager's discovery (by-id scan + sysfs
    attribute reads); overrides only add / get / remove of known devices.
    """

    def __init__(
        self, filter: DeviceFilter, node_name: str, in_cluster: bool = True
    ) -> None:
        self._filter = filter
        self._node = node_name
        # in_cluster: running as a pod (use the mounted ServiceAccount);
        # otherwise load the local ~/.kube/config (dev / out-of-cluster).
        if in_cluster:
            config.load_incluster_config()
        else:
            config.load_kube_config()
        self._api = client.CustomObjectsApi()

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
        return [_from_cr(o) for o in resp.get("items", [])]

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
                        "manufacturer": device.manufacturer,
                        "model": device.model,
                        "type": device.type.value,
                        "partition": device.partition,
                    },
                }
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
                },
            }
            self._api.create_cluster_custom_object(GROUP, VERSION, PLURAL, body)
            log.info("BlockDevice created: %s", device)
            return True

    def _remove_device_from_known_devices(self, device: HostBlockDevice) -> bool:
        # device is gone from the system -> delete its CR.
        name = _cr_name(device.serial)
        try:
            self._api.delete_cluster_custom_object(GROUP, VERSION, PLURAL, name)
        except ApiException as e:
            if e.status == 404:
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
        mngr = K8sUSBDeviceManager(filter=DeviceFilter(), node_name=str(node_name))
        # REVISIT: need to patch these inside k8s expect them as volumes
        mngr.USB_DEVDIR = environ["USB_DEVDIR"]
        mngr.SYS_CLASS_BLOCK = environ["SYS_CLASS_BLOCK"]
    except config.ConfigException:
        # device manager inside k8s didnt find k8s api
        mngr = K8sUSBDeviceManager(
            filter=DeviceFilter(), node_name=str(node_name), in_cluster=False
        )
    except KeyError as e:
        log.fatal(f"must set USB_DEVDIR and SYS_CLASS_BLOCK: {e}")
        exit(2)
    while True:
        mngr.reconcile()
        sleep(5)
