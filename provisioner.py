#!/usr/bin/env python3
"""USB local storage provisioner.

DaemonSet pod per node. Each pod:

  - Loads a list of *device models* (kernel-reported fields the controller
    knows about) from a mounted ConfigMap.
  - Detects USB block devices on its node by reading /host/sys/block and
    /host/proc/1/mountinfo. Matches each device against the registered
    models by vendor/model/serial/fs_uuid.
  - Records every recognised device in an etcd-backed ConfigMap registry,
    keyed by serial. Tracks first-seen / last-seen / current-node.
  - Initialises a blank (or minimally-populated) filesystem with the
    directory structure declared by its model, and drops an init marker.
  - Labels the node with `usb-storage.frankencluster.local/uuid-<UUID>=true`
    so the scheduler can place pods on nodes that have the right drive.
  - Provisions Kubernetes `local` PVs for PVCs whose StorageClass UUID is
    present on this node (WaitForFirstConsumer flow).
  - **Migrates** PVs when a device moves to a different node: the PV is
    deleted and recreated with the same name + claimRef but new
    nodeAffinity, and pods using the affected PVCs are evicted so their
    controllers reschedule them.
  - Emits Kubernetes Events for the major lifecycle moments
    (Provisioned, DeviceInserted, DeviceInitialized, DeviceRemoved,
    DeviceMigrated, PVMigrated).
  - Exposes /health on port 8080.
"""

import json
import logging
import os
import re
import shutil
import subprocess
import threading
import time
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer

import yaml
from kubernetes import client, config
from kubernetes.client.exceptions import ApiException

# --------------------------------------------------------------------------- #
# Constants
# --------------------------------------------------------------------------- #

PROVISIONER = "frankencluster.local/usb-storage"

UUID_LABEL_PREFIX = "usb-storage.frankencluster.local/uuid-"
SERIAL_LABEL = "usb-storage.frankencluster.local/serial"
NODE_LABEL = "usb-storage.frankencluster.local/node"
MODEL_LABEL = "usb-storage.frankencluster.local/model"

PROVISIONER_ANN = "volume.beta.kubernetes.io/storage-provisioner"
PROVISIONER_ANN_NEW = "volume.kubernetes.io/storage-provisioner"
SELECTED_NODE_ANN = "volume.kubernetes.io/selected-node"
PROVISIONED_BY_ANN = "pv.kubernetes.io/provisioned-by"

HOST_DATA = "/host/data"
HOST_BY_UUID = "/host/dev/disk/by-uuid"
HOST_SYS_BLOCK = "/host/sys/block"
HOST_MOUNTINFO = "/host/proc/1/mountinfo"

INIT_MARKER = ".usb-storage-init"

USBDEVICE_GROUP = "frankencluster.local"
USBDEVICE_VERSION = "v1"
USBDEVICE_PLURAL = "usbdevices"
USBDEVICE_KIND = "USBDevice"

DATA_MOUNTPOINT = "/data"  # canonical mountpoint on the host
NSENTER = ["nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--"]

NODE_NAME = os.environ["NODE_NAME"]
PROVISIONER_NS = os.environ.get("PROVISIONER_NAMESPACE", "dimsum")
MODELS_CONFIG_PATH = os.environ.get("MODELS_CONFIG", "/etc/usb-storage/models.yaml")
POLL_INTERVAL = int(os.environ.get("POLL_INTERVAL", "10"))
HEALTH_PORT = int(os.environ.get("HEALTH_PORT", "8080"))
HEALTH_STALE_AFTER = 60

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("usb")

_last_reconcile_ok = 0.0
_logged_unmatched = set()  # serials we've already logged as "no model matched"


# --------------------------------------------------------------------------- #
# Host operations (run via nsenter into PID 1's namespaces — uses host's
# binaries and host's mount namespace, so mounts/formats are "real").
# --------------------------------------------------------------------------- #

def host_run(cmd, check=True):
    full = NSENTER + cmd
    log.info("host$ %s", " ".join(full[len(NSENTER):]))
    result = subprocess.run(full, check=False, capture_output=True, text=True)
    if result.returncode != 0:
        log.warning("host cmd failed (rc=%d): %s\nstderr=%s",
                    result.returncode, " ".join(cmd), result.stderr.strip())
        if check:
            raise subprocess.CalledProcessError(
                result.returncode, full, result.stdout, result.stderr,
            )
    return result


def host_format(device, fs_label=None):
    """mkfs.ext4 on the device (destructive; only call when no fs is present)."""
    cmd = ["mkfs.ext4", "-F", "-q"]
    if fs_label:
        cmd += ["-L", fs_label[:16]]  # ext4 label max is 16 bytes
    cmd.append(device)
    host_run(cmd)


def host_mount(device, mountpoint):
    host_run(["mkdir", "-p", mountpoint])
    host_run(["mount", device, mountpoint])


def _data_mount_source():
    """Return the source device currently mounted at /data on the host, or None."""
    for source, mp in _host_mounts().items():
        if mp == DATA_MOUNTPOINT:
            return source
    return None


def ensure_ready(device, model):
    """Idempotently format (if no FS) and mount (if not mounted) the device.

    Updates `device` in place with the freshly discovered fs_uuid / mountpoint.
    Returns True if the device is now mounted at /data, False otherwise.
    """
    if device.get("mountpoint") == DATA_MOUNTPOINT:
        return True  # already where we want it

    init_spec = model.get("initialize", {}) or {}

    # Step 1: format if no filesystem is detected on the block device.
    if not device.get("fs_uuid"):
        if not init_spec.get("autoFormat", False):
            log.warning("device %s has no filesystem and model %s has autoFormat=false; skipping",
                        device["device"], model["name"])
            return False
        log.warning("FORMAT %s (model=%s, no existing filesystem)",
                    device["device"], model["name"])
        host_format(device["device"], fs_label=init_spec.get("fsLabel"))
        # Re-read UUID — udev should have populated /dev/disk/by-uuid by now.
        for _ in range(10):
            dev_to_uuid = {v: k for k, v in _uuid_to_device().items()}
            new_uuid = dev_to_uuid.get(device["device"])
            if new_uuid:
                device["fs_uuid"] = new_uuid
                break
            time.sleep(0.5)
        if not device.get("fs_uuid"):
            log.warning("device %s formatted but no UUID appeared in /dev/disk/by-uuid",
                        device["device"])
            return False

    # Step 2: mount at /data, unless something else is already there.
    busy_with = _data_mount_source()
    if busy_with and busy_with != device["device"]:
        log.warning("device %s wants /data but it is held by %s; skipping",
                    device["device"], busy_with)
        return False
    if not busy_with:
        log.warning("MOUNT %s at %s", device["device"], DATA_MOUNTPOINT)
        host_mount(device["device"], DATA_MOUNTPOINT)
    device["mountpoint"] = DATA_MOUNTPOINT
    return True


# --------------------------------------------------------------------------- #
# Device discovery (no external binaries — pure /sys + mountinfo reads)
# --------------------------------------------------------------------------- #

def _read(path):
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return ""


def _host_mounts():
    """Return {source_device: mountpoint} from the host's mount namespace."""
    out = {}
    try:
        with open(HOST_MOUNTINFO) as f:
            lines = f.read().splitlines()
    except OSError:
        return out
    for line in lines:
        parts = line.split()
        if "-" not in parts:
            continue
        dash = parts.index("-")
        # ID parent major:minor root mountpoint options ... - fstype source ...
        if len(parts) <= dash + 2 or dash < 5:
            continue
        mountpoint = parts[4]
        source = parts[dash + 2]
        out[source] = mountpoint
    return out


def _uuid_to_device():
    """Map filesystem UUID -> partition device path, e.g. '/dev/sda1'."""
    out = {}
    if not os.path.isdir(HOST_BY_UUID):
        return out
    for u in os.listdir(HOST_BY_UUID):
        try:
            out[u] = os.path.realpath(os.path.join(HOST_BY_UUID, u))
        except OSError:
            pass
    return out


def discover_devices():
    """Return a list of dicts describing the USB block devices on this node."""
    devices = []
    if not os.path.isdir(HOST_SYS_BLOCK):
        return devices

    mounts = _host_mounts()
    dev_to_uuid = {v: k for k, v in _uuid_to_device().items()}

    for name in sorted(os.listdir(HOST_SYS_BLOCK)):
        if name.startswith(("loop", "ram", "dm-", "sr", "fd", "nbd")):
            continue
        block_dir = os.path.join(HOST_SYS_BLOCK, name)
        # Only USB-attached devices — /sys symlink to /sys/devices/.../usbN/...
        if "usb" not in os.path.realpath(block_dir):
            continue

        vendor = _read(os.path.join(block_dir, "device", "vendor"))
        model = _read(os.path.join(block_dir, "device", "model"))
        serial = (
            _read(os.path.join(block_dir, "serial"))
            or _read(os.path.join(block_dir, "device", "serial"))
        )

        partitions = [
            e for e in os.listdir(block_dir)
            if e.startswith(name) and e != name
            and os.path.isdir(os.path.join(block_dir, e))
        ]
        if not partitions:
            partitions = [name]  # whole-disk filesystem

        for part in partitions:
            part_dev = f"/dev/{part}"
            devices.append({
                "device": part_dev,
                "vendor": vendor,
                "model": model,
                "serial": serial,
                "fs_uuid": dev_to_uuid.get(part_dev),
                "mountpoint": mounts.get(part_dev),
            })
    return devices


# --------------------------------------------------------------------------- #
# Device models
# --------------------------------------------------------------------------- #

def load_models():
    if not os.path.exists(MODELS_CONFIG_PATH):
        log.warning("models config %s missing; no devices will be registered",
                    MODELS_CONFIG_PATH)
        return []
    with open(MODELS_CONFIG_PATH) as f:
        data = yaml.safe_load(f) or {}
    models = data.get("models", []) or []
    log.info("loaded %d device model(s): %s",
             len(models), [m.get("name", "?") for m in models])
    return models


def match_model(device, models):
    """First model whose match-dict matches all the fields the device exposes."""
    for m in models:
        criteria = m.get("match", {}) or {}
        if not criteria:
            continue
        if all(device.get(k, "") == v for k, v in criteria.items()):
            return m
    return None


# --------------------------------------------------------------------------- #
# Registry (etcd-backed via the USBDevice CRD)
# --------------------------------------------------------------------------- #

def _serial_to_name(serial):
    """Sanitize a USB serial into a valid Kubernetes resource name."""
    name = re.sub(r"[^a-z0-9.-]", "-", serial.lower())
    name = re.sub(r"-+", "-", name).strip("-.")
    return (name[:253] or "unknown-serial")


def registry_load(co):
    """Return {serial: dict} for all USBDevice CRs in the cluster."""
    try:
        lst = co.list_cluster_custom_object(
            USBDEVICE_GROUP, USBDEVICE_VERSION, USBDEVICE_PLURAL,
        )
    except ApiException as e:
        if e.status == 404:
            return {}
        raise
    entries = {}
    for item in lst.get("items", []):
        spec = item.get("spec") or {}
        status = item.get("status") or {}
        serial = spec.get("serial")
        if not serial:
            continue
        entries[serial] = {
            "serial": serial,
            "model": spec.get("model"),
            "vendor": spec.get("vendor"),
            "modelString": spec.get("modelString"),
            "firstSeen": spec.get("firstSeen"),
            "fsUUID": status.get("fsUUID"),
            "currentNode": status.get("currentNode"),
            "lastSeen": status.get("lastSeen"),
            "phase": status.get("phase", "Present"),
            "mountpoint": status.get("mountpoint"),
        }
    return entries


def registry_upsert(co, serial, entry):
    """Create-or-update the USBDevice CR for `serial`. Status is patched separately."""
    name = _serial_to_name(serial)
    spec_body = {
        "apiVersion": f"{USBDEVICE_GROUP}/{USBDEVICE_VERSION}",
        "kind": USBDEVICE_KIND,
        "metadata": {"name": name},
        "spec": {
            "serial": serial,
            "model": entry.get("model"),
            "vendor": entry.get("vendor"),
            "modelString": entry.get("modelString"),
            "firstSeen": entry.get("firstSeen"),
        },
    }
    status_body = {
        "status": {
            "fsUUID": entry.get("fsUUID"),
            "currentNode": entry.get("currentNode"),
            "lastSeen": entry.get("lastSeen"),
            "phase": entry.get("phase", "Present"),
            "mountpoint": entry.get("mountpoint"),
        }
    }

    try:
        co.get_cluster_custom_object(
            USBDEVICE_GROUP, USBDEVICE_VERSION, USBDEVICE_PLURAL, name,
        )
        co.patch_cluster_custom_object(
            USBDEVICE_GROUP, USBDEVICE_VERSION, USBDEVICE_PLURAL, name, spec_body,
        )
    except ApiException as e:
        if e.status != 404:
            raise
        co.create_cluster_custom_object(
            USBDEVICE_GROUP, USBDEVICE_VERSION, USBDEVICE_PLURAL, spec_body,
        )

    co.patch_cluster_custom_object_status(
        USBDEVICE_GROUP, USBDEVICE_VERSION, USBDEVICE_PLURAL, name, status_body,
    )


# --------------------------------------------------------------------------- #
# Kubernetes events
# --------------------------------------------------------------------------- #

def emit_event(v1, reason, message, regarding=None, type_="Normal"):
    now = datetime.now(timezone.utc)
    name = f"usb-storage.{reason.lower()}.{int(now.timestamp())}.{os.urandom(2).hex()}"
    involved = regarding or client.V1ObjectReference(
        api_version="v1", kind="Node", name=NODE_NAME,
    )
    event = client.CoreV1Event(
        metadata=client.V1ObjectMeta(name=name, namespace=PROVISIONER_NS),
        type=type_,
        reason=reason,
        message=message,
        source=client.V1EventSource(component="usb-storage-provisioner", host=NODE_NAME),
        first_timestamp=now,
        last_timestamp=now,
        involved_object=involved,
    )
    try:
        v1.create_namespaced_event(PROVISIONER_NS, event)
    except ApiException:
        log.exception("failed to emit event %s", reason)


# --------------------------------------------------------------------------- #
# Initialization
# --------------------------------------------------------------------------- #

def _host_view(mountpoint):
    """Convert a host mountpoint (e.g. /data) to its in-container path."""
    if not mountpoint:
        return None
    return os.path.join("/host", mountpoint.lstrip("/"))


def is_blank(mountpoint):
    """A filesystem is 'blank' if it has no init marker and no payload."""
    path = _host_view(mountpoint)
    if not path or not os.path.isdir(path):
        return False
    if os.path.exists(os.path.join(path, INIT_MARKER)):
        return False
    entries = [e for e in os.listdir(path) if e != "lost+found"]
    return len(entries) == 0


def initialize_device(mountpoint, model, serial):
    path = _host_view(mountpoint)
    if not path:
        return
    init_spec = model.get("initialize", {}) or {}
    for d in init_spec.get("directories", []) or []:
        os.makedirs(os.path.join(path, d), exist_ok=True)
    with open(os.path.join(path, INIT_MARKER), "w") as f:
        json.dump({
            "serial": serial,
            "model": model.get("name"),
            "initialized": datetime.now(timezone.utc).isoformat(),
            "node": NODE_NAME,
        }, f, indent=2)


# --------------------------------------------------------------------------- #
# Node labels
# --------------------------------------------------------------------------- #

def update_node_labels(v1, fs_uuids):
    desired = {f"{UUID_LABEL_PREFIX}{u}": "true" for u in fs_uuids if u}
    node = v1.read_node(NODE_NAME)
    current = {
        k: v for k, v in (node.metadata.labels or {}).items()
        if k.startswith(UUID_LABEL_PREFIX)
    }
    patch = {}
    for k, v in desired.items():
        if current.get(k) != v:
            patch[k] = v
    for k in current:
        if k not in desired:
            patch[k] = None
    if patch:
        v1.patch_node(NODE_NAME, {"metadata": {"labels": patch}})
        log.info("node labels updated: %s", patch)


# --------------------------------------------------------------------------- #
# PV provisioning
# --------------------------------------------------------------------------- #

def _matches_our_provisioner(annotations):
    return (annotations.get(PROVISIONER_ANN) == PROVISIONER
            or annotations.get(PROVISIONER_ANN_NEW) == PROVISIONER)


def _build_pv(pvc, sc, sc_uuid, node, serial, model_name):
    pv_name = f"usb-{pvc.metadata.namespace}-{pvc.metadata.name}-{pvc.metadata.uid[:8]}"
    labels = {
        "usb-storage.frankencluster.local/uuid": sc_uuid,
        NODE_LABEL: node,
    }
    if serial:
        labels[SERIAL_LABEL] = serial
    if model_name:
        labels[MODEL_LABEL] = model_name

    return client.V1PersistentVolume(
        metadata=client.V1ObjectMeta(
            name=pv_name,
            annotations={PROVISIONED_BY_ANN: PROVISIONER},
            labels=labels,
        ),
        spec=client.V1PersistentVolumeSpec(
            capacity={"storage": pvc.spec.resources.requests["storage"]},
            access_modes=pvc.spec.access_modes or ["ReadWriteOnce"],
            persistent_volume_reclaim_policy=sc.reclaim_policy or "Delete",
            storage_class_name=sc.metadata.name,
            volume_mode=pvc.spec.volume_mode or "Filesystem",
            local=client.V1LocalVolumeSource(path=f"/data/{pvc.metadata.name}"),
            node_affinity=_node_affinity(node),
            claim_ref=client.V1ObjectReference(
                api_version="v1", kind="PersistentVolumeClaim",
                namespace=pvc.metadata.namespace, name=pvc.metadata.name,
                uid=pvc.metadata.uid,
                resource_version=pvc.metadata.resource_version,
            ),
        ),
    )


def _node_affinity(node):
    return client.V1VolumeNodeAffinity(
        required=client.V1NodeSelector(
            node_selector_terms=[
                client.V1NodeSelectorTerm(
                    match_expressions=[
                        client.V1NodeSelectorRequirement(
                            key="kubernetes.io/hostname", operator="In",
                            values=[node],
                        )
                    ]
                )
            ]
        )
    )


def provision_pvc(v1, storage_v1, pvc, fs_uuid_to_device):
    if pvc.spec.volume_name:
        return
    sc_name = pvc.spec.storage_class_name
    if not sc_name:
        return
    try:
        sc = storage_v1.read_storage_class(sc_name)
    except ApiException as e:
        if e.status == 404:
            return
        raise
    if sc.provisioner != PROVISIONER:
        return
    sc_uuid = (sc.parameters or {}).get("uuid")
    if not sc_uuid:
        log.warning("StorageClass %s missing 'uuid' parameter", sc_name)
        return
    if sc_uuid not in fs_uuid_to_device:
        return

    selected = (pvc.metadata.annotations or {}).get(SELECTED_NODE_ANN)
    if selected and selected != NODE_NAME:
        return

    device = fs_uuid_to_device[sc_uuid]
    target = os.path.join(HOST_DATA, pvc.metadata.name)
    os.makedirs(target, exist_ok=True)

    pv = _build_pv(pvc, sc, sc_uuid, NODE_NAME,
                   device.get("serial"), device.get("model_name"))
    try:
        v1.create_persistent_volume(pv)
    except ApiException as e:
        if e.status == 409:
            return
        raise

    log.info("provisioned PV %s for PVC %s/%s on node %s (device serial %s)",
             pv.metadata.name, pvc.metadata.namespace, pvc.metadata.name,
             NODE_NAME, device.get("serial"))
    emit_event(
        v1, "Provisioned",
        f"PV {pv.metadata.name} provisioned on {NODE_NAME} "
        f"(device serial {device.get('serial')}, model {device.get('model_name')})",
        regarding=client.V1ObjectReference(
            api_version="v1", kind="PersistentVolumeClaim",
            namespace=pvc.metadata.namespace, name=pvc.metadata.name,
            uid=pvc.metadata.uid,
        ),
    )


def cleanup_pv(v1, pv):
    if pv.spec.persistent_volume_reclaim_policy != "Delete":
        return
    if pv.status.phase != "Released":
        return
    if not _pv_pinned_to(pv, NODE_NAME):
        return

    if pv.spec.local and pv.spec.local.path:
        rel = os.path.relpath(pv.spec.local.path, "/data")
        target = os.path.join(HOST_DATA, rel)
        if (os.path.commonpath([os.path.abspath(target), HOST_DATA]) == HOST_DATA
                and os.path.isdir(target)):
            shutil.rmtree(target, ignore_errors=True)
            log.info("removed data dir %s", target)

    try:
        v1.delete_persistent_volume(pv.metadata.name)
        log.info("deleted PV %s", pv.metadata.name)
    except ApiException as e:
        if e.status != 404:
            raise


def _pv_pinned_to(pv, node):
    aff = pv.spec.node_affinity
    if not aff or not aff.required:
        return False
    for t in aff.required.node_selector_terms or []:
        for e in t.match_expressions or []:
            if e.key == "kubernetes.io/hostname" and node in (e.values or []):
                return True
    return False


def _pv_pinned_node(pv):
    aff = pv.spec.node_affinity
    if not aff or not aff.required:
        return None
    for t in aff.required.node_selector_terms or []:
        for e in t.match_expressions or []:
            if e.key == "kubernetes.io/hostname":
                return (e.values or [None])[0]
    return None


# --------------------------------------------------------------------------- #
# Migration: device moved to a different node
# --------------------------------------------------------------------------- #

def migrate_device(v1, serial, old_node, new_node):
    """Move every PV labelled with this serial from old_node to new_node.

    Pods using the affected PVCs are deleted so their controllers reschedule
    them onto the new node. Logged loudly because this is a user-visible event.
    """
    pvs = v1.list_persistent_volume(
        label_selector=f"{SERIAL_LABEL}={serial}",
    ).items
    targets = [pv for pv in pvs if _pv_pinned_node(pv) == old_node]
    if not targets:
        log.warning("MIGRATION serial=%s: %s -> %s, no PVs to move",
                    serial, old_node, new_node)
        return 0

    log.warning("MIGRATION serial=%s: %s -> %s, %d PV(s) to move",
                serial, old_node, new_node, len(targets))

    # 1. Evict pods using affected PVCs first so the mount is no longer held.
    for pv in targets:
        cr = pv.spec.claim_ref
        if not cr:
            continue
        try:
            pods = v1.list_namespaced_pod(cr.namespace).items
        except ApiException:
            continue
        for pod in pods:
            for vol in pod.spec.volumes or []:
                pvc_ref = vol.persistent_volume_claim
                if pvc_ref and pvc_ref.claim_name == cr.name:
                    log.warning("MIGRATION evicting pod %s/%s (uses PVC %s)",
                                pod.metadata.namespace, pod.metadata.name,
                                cr.name)
                    try:
                        v1.delete_namespaced_pod(
                            pod.metadata.name, pod.metadata.namespace,
                            grace_period_seconds=0,
                        )
                    except ApiException as e:
                        if e.status != 404:
                            log.warning("could not delete pod %s/%s: %s",
                                        pod.metadata.namespace,
                                        pod.metadata.name, e)
                    break

    # 2. Delete and recreate each PV with the new node affinity. PV nodeAffinity
    #    is immutable for local volumes, so we have to recreate. The pv-protection
    #    finalizer would block deletion while bound — we strip it manually.
    migrated = 0
    for pv in targets:
        name = pv.metadata.name
        new_labels = dict(pv.metadata.labels or {})
        new_labels[NODE_LABEL] = new_node
        replacement = client.V1PersistentVolume(
            metadata=client.V1ObjectMeta(
                name=name, labels=new_labels, annotations=pv.metadata.annotations,
            ),
            spec=client.V1PersistentVolumeSpec(
                capacity=pv.spec.capacity,
                access_modes=pv.spec.access_modes,
                persistent_volume_reclaim_policy=pv.spec.persistent_volume_reclaim_policy,
                storage_class_name=pv.spec.storage_class_name,
                volume_mode=pv.spec.volume_mode,
                local=pv.spec.local,
                node_affinity=_node_affinity(new_node),
                claim_ref=client.V1ObjectReference(
                    api_version="v1", kind="PersistentVolumeClaim",
                    namespace=pv.spec.claim_ref.namespace,
                    name=pv.spec.claim_ref.name,
                    uid=pv.spec.claim_ref.uid,
                ) if pv.spec.claim_ref else None,
            ),
        )

        try:
            v1.patch_persistent_volume(name, {"metadata": {"finalizers": []}})
        except ApiException:
            pass
        try:
            v1.delete_persistent_volume(name)
        except ApiException as e:
            if e.status != 404:
                log.warning("MIGRATION delete PV %s failed: %s", name, e)
                continue

        # Wait for the delete to actually finish before recreating with the same name.
        for _ in range(30):
            try:
                v1.read_persistent_volume(name)
                time.sleep(1)
            except ApiException as e:
                if e.status == 404:
                    break

        try:
            v1.create_persistent_volume(replacement)
        except ApiException as e:
            log.warning("MIGRATION recreate PV %s failed: %s", name, e)
            continue

        migrated += 1
        log.warning("MIGRATION recreated PV %s pinned to %s", name, new_node)
        emit_event(
            v1, "PVMigrated",
            f"PV {name} migrated from {old_node} to {new_node} "
            f"(device serial {serial})",
            type_="Warning",
            regarding=client.V1ObjectReference(
                api_version="v1", kind="PersistentVolume", name=name,
            ),
        )

    return migrated


# --------------------------------------------------------------------------- #
# Reconcile
# --------------------------------------------------------------------------- #

def reconcile(v1, storage_v1, co, models):
    devices = discover_devices()

    # Tag each device with its matched model.
    for d in devices:
        m = match_model(d, models)
        d["model_name"] = m["name"] if m else None
        d["_model"] = m
        if not m:
            serial = d.get("serial") or d["device"]
            if serial not in _logged_unmatched:
                log.info("ignored device %s (no model matched): vendor=%r model=%r serial=%s fs=%s",
                         d["device"], d.get("vendor"), d.get("model"),
                         d.get("serial"), d.get("fs_uuid"))
                _logged_unmatched.add(serial)

    # Format + mount matching devices before doing anything else. Sorted by
    # device path so the first matching drive wins /data deterministically.
    for d in sorted([x for x in devices if x.get("_model")],
                    key=lambda x: x["device"]):
        try:
            ensure_ready(d, d["_model"])
        except Exception:
            log.exception("ensure_ready failed for %s", d["device"])

    fs_uuids = {d["fs_uuid"] for d in devices if d.get("fs_uuid")}
    update_node_labels(v1, fs_uuids)

    # Registry update + insertion / initialization / migration detection.
    registry = registry_load(co)
    now = datetime.now(timezone.utc).isoformat()
    local_serials = set()

    for d in devices:
        serial = d.get("serial")
        if not serial:
            continue  # can't track devices without a serial
        local_serials.add(serial)
        m = d["_model"]
        existing = registry.get(serial)

        # Migration: this serial was last seen on a different node.
        if existing and existing.get("currentNode") and existing["currentNode"] != NODE_NAME:
            old_node = existing["currentNode"]
            log.warning("MIGRATION device serial=%s moved %s -> %s",
                        serial, old_node, NODE_NAME)
            emit_event(
                v1, "DeviceMigrated",
                f"Device serial {serial} (model {existing.get('model')}) "
                f"moved from {old_node} to {NODE_NAME}",
                type_="Warning",
            )
            migrate_device(v1, serial, old_node, NODE_NAME)

        entry = {
            "serial": serial,
            "model": m["name"] if m else None,
            "vendor": d.get("vendor"),
            "modelString": d.get("model"),
            "fsUUID": d.get("fs_uuid"),
            "currentNode": NODE_NAME,
            "lastSeen": now,
            "firstSeen": (existing or {}).get("firstSeen", now),
            "phase": "Present",
            "mountpoint": d.get("mountpoint"),
        }

        if not existing:
            log.info("device inserted: model=%s serial=%s fs=%s vendor=%r product=%r node=%s",
                     entry["model"], serial, entry["fsUUID"],
                     d.get("vendor"), d.get("model"), NODE_NAME)
            emit_event(
                v1, "DeviceInserted",
                f"Device {entry['model'] or 'unrecognized'} (serial {serial}) "
                f"inserted on {NODE_NAME}",
            )

        registry_upsert(co, serial, entry)

        # Initialization: only if a model matched, device is mounted, and it's blank.
        if m and d.get("mountpoint") and is_blank(d["mountpoint"]):
            log.info("initializing device model=%s serial=%s at %s",
                     m["name"], serial, d["mountpoint"])
            try:
                initialize_device(d["mountpoint"], m, serial)
                emit_event(
                    v1, "DeviceInitialized",
                    f"Initialized device {m['name']} (serial {serial}) "
                    f"at {d['mountpoint']} on {NODE_NAME}",
                )
            except OSError:
                log.exception("initialization failed for serial %s", serial)

    # Removal: registry says the device is here, but we don't see it any more.
    for serial, entry in list(registry.items()):
        if (entry.get("currentNode") == NODE_NAME
                and entry.get("phase") != "Removed"
                and serial not in local_serials):
            log.info("device removed: model=%s serial=%s (was on %s)",
                     entry.get("model"), serial, NODE_NAME)
            emit_event(
                v1, "DeviceRemoved",
                f"Device serial {serial} (model {entry.get('model')}) "
                f"removed from {NODE_NAME}",
            )
            entry["phase"] = "Removed"
            entry["lastSeen"] = now
            # Keep currentNode set so another node sees "moved from X" on reappear.
            registry_upsert(co, serial, entry)

    # PVC provisioning + PV cleanup.
    fs_uuid_to_device = {d["fs_uuid"]: d for d in devices if d.get("fs_uuid")}
    for pvc in v1.list_persistent_volume_claim_for_all_namespaces().items:
        if not _matches_our_provisioner(pvc.metadata.annotations or {}):
            continue
        try:
            provision_pvc(v1, storage_v1, pvc, fs_uuid_to_device)
        except Exception:
            log.exception("provisioning %s/%s failed",
                          pvc.metadata.namespace, pvc.metadata.name)

    for pv in v1.list_persistent_volume().items:
        if (pv.metadata.annotations or {}).get(PROVISIONED_BY_ANN) != PROVISIONER:
            continue
        try:
            cleanup_pv(v1, pv)
        except Exception:
            log.exception("cleanup of PV %s failed", pv.metadata.name)


# --------------------------------------------------------------------------- #
# Health server
# --------------------------------------------------------------------------- #

class _HealthHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/health":
            self.send_response(404); self.end_headers(); return
        fresh = (time.time() - _last_reconcile_ok) < HEALTH_STALE_AFTER
        self.send_response(200 if fresh else 503)
        self.send_header("Content-Type", "text/plain"); self.end_headers()
        self.wfile.write(b"ok\n" if fresh else b"stale\n")

    def log_message(self, *_args, **_kwargs):
        pass


def start_health_server():
    server = HTTPServer(("0.0.0.0", HEALTH_PORT), _HealthHandler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    log.info("health server listening on :%d", HEALTH_PORT)


# --------------------------------------------------------------------------- #
# Main
# --------------------------------------------------------------------------- #

def main():
    config.load_incluster_config()
    v1 = client.CoreV1Api()
    storage_v1 = client.StorageV1Api()
    co = client.CustomObjectsApi()
    models = load_models()
    log.info("usb-storage provisioner starting on node %s with %d models",
             NODE_NAME, len(models))
    start_health_server()

    global _last_reconcile_ok
    while True:
        try:
            reconcile(v1, storage_v1, co, models)
            _last_reconcile_ok = time.time()
        except Exception:
            log.exception("reconcile error")
        time.sleep(POLL_INTERVAL)


if __name__ == "__main__":
    main()
