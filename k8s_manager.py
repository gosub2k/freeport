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

import logging
import re
import subprocess
from threading import Thread
from time import sleep

from kubernetes import client, config
from kubernetes.client.exceptions import ApiException

from abstract_manager import BlockDeviceType, DeviceFilter, HostBlockDevice, Serial, log
from inmemory_manager import InMemoryUSBDeviceManager

GROUP = "freeport.local"
VERSION = "v1alpha1"
PLURAL = "blockdevices"

MY_PROVISIOER = "local/freeport"


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


class K8sUSBDeviceManager(InMemoryUSBDeviceManager, Thread):
    """Persists the known-device registry as BlockDevice custom resources.

    Inherits all of InMemoryUSBDeviceManager's discovery (by-id scan + sysfs
    attribute reads); overrides only add / get / remove of known devices.
    """

    def __init__(
        self,
        sc: client.V1StorageClass,
        node_name: str,
        in_cluster: bool = True,
        usb_devdir: str | None = None,
        sys_class_block: str | None = None,
        host_mount: str | None = None,
    ) -> None:
        Thread.__init__(self, daemon=True)
        params = sc.parameters
        self._filter = DeviceFilter(  # Needed by superclass
            manufacterer=params.get("manufacterer", ""),
            model=params.get("model", ""),
            serial=params.get("serial", ""),
            type=params.get("type", "usb"),
        )
        self._sc = sc
        self._node = node_name
        self._USB_DEVDIR = usb_devdir or self._USB_DEVDIR
        self._SYS_CLASS_BLOCK = sys_class_block or self._SYS_CLASS_BLOCK
        self._HOST_MOUNT = host_mount or self._HOST_MOUNT
        self._api = client.CustomObjectsApi()
        self._running = True
        self._log = logging.LoggerAdapter(log, {"sc": self._sc.metadata.name})

    @property
    def log(self):
        return self._log

    def replace_dev_path(self, dev_path):
        return re.sub(r"/mnt", self._HOST_MOUNT, dev_path)

    # REVISIT: use a different run hook below
    def mount(self, dev_path: str, serial: str) -> str:
        def nsenter_run(cmd, *args, **kwargs):
            _NSENTER = ["nsenter", "--target", "1", "--mount", "--"]
            return tmp_run(_NSENTER + list(cmd), *args, **kwargs)

        tmp_run = subprocess.run
        subprocess.run = nsenter_run
        dev_path = self.replace_dev_path(dev_path)
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
            self.log.exception("listing BlockDevices failed")
            return []
        return [
            _from_cr(o)
            for o in resp.get("items", [])
            if o.get("spec").get("node") == self._node
            and o.get("spec").get("class") == self._sc.metadata.name
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
                        # Expect mountpoint and free(?) to change on a new node.
                        "node": self._node,
                        "mountpoint": device.mountpoint,
                        "free": device.free,
                    },
                }
            # TODO - make this a CAS operation
            self._api.patch_cluster_custom_object(GROUP, VERSION, PLURAL, name, patch)
            prev_node = existing.get("spec").get("node", "")
            self.log.info("BlockDevice updated (node %s -> %s): %s", prev_node, self._node, device)
            return True

        else:
            body = {
                "apiVersion": f"{GROUP}/{VERSION}",
                "kind": "BlockDevice",
                "metadata": {
                    "name": name,
                },
                "spec": {
                    "class": self._sc.metadata.name,
                    "node": self._node,
                    "manufacturer": device.manufacturer,
                    "model": device.model,
                    "serial": str(device.serial),
                    "type": device.type.value,
                    "partition": device.partition,
                    "free": device.free,
                    "mountpoint": device.mountpoint,
                },
            }
            self._api.create_cluster_custom_object(GROUP, VERSION, PLURAL, body)
            self.log.info("BlockDevice created: %s", device)
            return True

    def _refresh_device_info(self, dev) -> None:
        return super()._refresh_device_info(dev)

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
            self.log.info(
                "not deleting BlockDevice %s: belongs to node %s, not us (%s)",
                name, existing.get("spec").get("node"), self._node,
            )
            return False
        if existing.get("spec").get("class") != self._sc.metadata.name:
            self.log.info(
                "not deleting BlockDevice %s: belongs to SC %s, not us",
                name, existing.get("spec").get("class"),
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
        self.log.info("BlockDevice deleted: %s", device)
        return True

    def stop(self):
        self._running = False

    def run(self):
        while self._running:
            self.reconcile()
            sleep(5)


def device_matches_sc(device: HostBlockDevice, sc: client.V1StorageClass) -> bool:
    """True if `device` satisfies every match field set in `sc.parameters`.

    Supported parameters:
      vendor   — fnmatch glob against /sys/.../device/vendor
      model    — fnmatch glob against /sys/.../device/model
      serials  — comma-separated allowlist; the device serial must be in it
    At least one of these must be set; otherwise the StorageClass matches
    nothing (refuse implicit match-everything).
    """
    p = sc.parameters or {}
    has_criteria = False
    if "vendor" in p and device.vendor is not None:
        if not fnmatch.fnmatchcase(device.vendor, p["vendor"]):
            return False
        has_criteria = True
    if "model" in p and device.model is not None:
        if not fnmatch.fnmatchcase(device.model, p["model"]):
            return False
        has_criteria = True
    if "serials" in p and device.serial is not None:
        if device.serial not in _parse_serials(p["serials"]):
            return False
        has_criteria = True
    return has_criteria


def get_manager(sc: client.V1StorageClass) -> K8sUSBDeviceManager:
    from os import environ

    node_name = environ.get("NODE_NAME")
    if node_name is None:
        log.fatal("NODE_NAME not defined")
        exit(1)
    usb_devdir = environ.get("USB_DEVDIR")
    if usb_devdir is None:
        log.fatal("USB_DEVDIR not set")
        exit(2)
    sys_class_block = environ.get("SYS_CLASS_BLOCK")
    if sys_class_block is None:
        log.fatal("SYS_CLASS_BLOCK not set")
        exit(2)
    host_mount = environ.get("HOST_MOUNT")
    if host_mount is None:
        log.fatal("HOST_MOUNT not set")
        exit(2)

    return K8sUSBDeviceManager(
        sc=sc,
        node_name=str(node_name),
        usb_devdir=usb_devdir,
        sys_class_block=sys_class_block,
        host_mount=host_mount,
    )


def watch_scs():
    # sc_name -> (manager, resourceVersion at start time)
    sc_threads: dict[str, tuple[K8sUSBDeviceManager, str]] = dict()
    scapi = client.StorageV1Api()

    while True:
        scs = scapi.list_storage_class().items
        for sc in scs:
            if sc.provisioner != MY_PROVISIOER:
                continue
            sc_name = sc.metadata.name
            rv = sc.metadata.resource_version

            existing = sc_threads.get(sc_name)
            if existing is not None:
                _, known_rv = existing
                if rv != known_rv:
                    log.info(
                        "StorageClass %s changed (rv %s -> %s) — restarting manager",
                        sc_name,
                        known_rv,
                        rv,
                    )
                    sc_threads.pop(sc_name)[0].stop()
                else:
                    continue

            log.info("new StorageClass detected: %s (rv=%s)", sc_name, rv)
            mngr = get_manager(sc)
            mngr.start()
            sc_threads[sc_name] = (mngr, rv)

        # Stop managers for SCs that were deleted or changed provisioner.
        current_names = {
            sc.metadata.name for sc in scs if sc.provisioner == MY_PROVISIOER
        }
        for sc_name in list(sc_threads):
            if sc_name not in current_names:
                log.info("StorageClass %s removed — stopping manager", sc_name)
                sc_threads.pop(sc_name)[0].stop()

        sleep(2)


if __name__ == "__main__":
    try:
        config.load_incluster_config()
        log.info("loaded in cluster k8s config")
    except:
        config.load_config()
        log.info("loaded local k8s config")

    watch_scs()
