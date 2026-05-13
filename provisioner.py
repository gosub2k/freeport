#!/usr/bin/env python3
"""USB local storage provisioner.

DaemonSet pod per node. Each pod:

  * Discovers USB block devices by reading /host/sys/block and
    /host/proc/1/mountinfo (no external binaries needed).
  * Reads every StorageClass whose provisioner is ours. Match criteria
    live in each StorageClass's `parameters`: vendor (fnmatch glob),
    model (fnmatch glob), serials (comma-separated allowlist). All set
    fields must pass.
  * For a matched, unformatted device whose StorageClass has
    autoFormat=true, runs mkfs.ext4 via nsenter on the host. Then mounts
    the device at /data, also via nsenter.
  * Records each device as an annotation on every matching StorageClass:
    `usb-storage.frankencluster.local/dev.<sanitised-serial>` → JSON
    payload (vendor, model, serial, node, fsUUID, mountpoint, phase,
    first/last-seen). Migration detection cross-references these.
  * Labels the node with `usb-storage.frankencluster.local/class-<scname>`
    for every StorageClass that has a ready local device.
  * Provisions Kubernetes `local` PVs (path /data/<pvc-name>) when a PVC
    references one of our StorageClasses and a matching device is locally
    mounted (WaitForFirstConsumer flow).
  * Migrates PVs when a drive moves to a different node: the PV is
    deleted and recreated with the same name + claimRef but new
    nodeAffinity, and pods using the affected PVCs are evicted so their
    controllers reschedule them.
  * Emits Kubernetes Events for the major lifecycle moments
    (Provisioned, DeviceInserted, DeviceRemoved, DeviceMigrated, PVMigrated).
  * Exposes /health on port 8080.
"""

import fnmatch
import json
import logging
import os
import re
import shutil
import subprocess
import threading
import time
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer

from kubernetes import client, config
from kubernetes.client.exceptions import ApiException


# --------------------------------------------------------------------------- #
# Constants
# --------------------------------------------------------------------------- #

PROVISIONER = "frankencluster.local/usb-storage"

ANN_DOMAIN = "usb-storage.frankencluster.local"
DEVICE_ANN_PREFIX = f"{ANN_DOMAIN}/dev."        # per-device annotation on each SC

CLASS_LABEL_PREFIX = f"{ANN_DOMAIN}/class-"     # node label: SC has local match
SERIAL_LABEL = f"{ANN_DOMAIN}/serial"           # PV label: device serial
NODE_LABEL = f"{ANN_DOMAIN}/node"               # PV label: pinned node
CLASS_LABEL = f"{ANN_DOMAIN}/class"             # PV label: matched SC name

PROVISIONER_ANN = "volume.beta.kubernetes.io/storage-provisioner"
PROVISIONER_ANN_NEW = "volume.kubernetes.io/storage-provisioner"
SELECTED_NODE_ANN = "volume.kubernetes.io/selected-node"
PROVISIONED_BY_ANN = "pv.kubernetes.io/provisioned-by"

HOST_DATA = "/host/data"
HOST_SYS_BLOCK = "/host/sys/block"
HOST_MOUNTINFO = "/host/proc/1/mountinfo"
HOST_BY_UUID = "/host/dev/disk/by-uuid"

DATA_MOUNTPOINT = "/data"
NSENTER = ["nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--"]

NODE_NAME = os.environ["NODE_NAME"]
PROVISIONER_NS = os.environ.get("PROVISIONER_NAMESPACE", "dimsum")
POLL_INTERVAL = int(os.environ.get("POLL_INTERVAL", "10"))
HEALTH_PORT = int(os.environ.get("HEALTH_PORT", "8080"))
HEALTH_STALE_AFTER = 60


logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("usb")


# --------------------------------------------------------------------------- #
# Types
# --------------------------------------------------------------------------- #


class Serial(str):
    """Typed alias for USB serial numbers."""


@dataclass
class HostBlockDevice:
    """A USB block device as seen by the kernel on this node."""
    device: str
    vendor: str
    model: str
    serial: Serial
    fs_uuid: str | None
    mountpoint: str | None


@dataclass
class RegisteredDevice:
    """Persisted form of a known device — written as a JSON-encoded
    annotation value on every StorageClass that matches it."""
    serial: str
    vendor: str
    model: str
    fs_uuid: str | None
    node: str
    mountpoint: str | None
    first_seen: str
    last_seen: str
    phase: str  # "Present" or "Removed"

    @classmethod
    def from_json(cls, blob: str) -> "RegisteredDevice":
        return cls(**json.loads(blob))

    def to_json(self) -> str:
        return json.dumps(asdict(self), sort_keys=True)


# --------------------------------------------------------------------------- #
# Provisioner
# --------------------------------------------------------------------------- #


class UsbStorageProvisioner:

    def __init__(self) -> None:
        config.load_incluster_config()
        self._v1 = client.CoreV1Api()
        self._storage_v1 = client.StorageV1Api()

        self._raw_system_devices: list[HostBlockDevice] = []
        self._available_devices: dict[Serial, HostBlockDevice] = {}
        self._matched_scs: dict[Serial, list[client.V1StorageClass]] = {}
        self._storage_classes: list[client.V1StorageClass] = []

        self._pending_migrations: set[tuple[Serial, str]] = set()  # (serial, old_node)
        self._logged_unmatched: set[Serial] = set()
        self._last_reconcile_ok: float = 0.0

    # ---- system device discovery ------------------------------------------ #

    def _refresh_system_devices(self) -> None:
        def read(path: str) -> str:
            try:
                with open(path) as f:
                    return f.read().strip()
            except OSError:
                return ""

        def host_mounts() -> dict[str, str]:
            """Return {source_device: mountpoint} from the host's mount namespace."""
            out: dict[str, str] = {}
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
                if dash < 5 or len(parts) <= dash + 2:
                    continue
                out[parts[dash + 2]] = parts[4]
            return out

        def uuid_to_device() -> dict[str, str]:
            out: dict[str, str] = {}
            if not os.path.isdir(HOST_BY_UUID):
                return out
            for u in os.listdir(HOST_BY_UUID):
                try:
                    out[u] = os.path.realpath(os.path.join(HOST_BY_UUID, u))
                except OSError:
                    pass
            return out

        def is_usb(block_dir: str) -> bool:
            return "usb" in os.path.realpath(block_dir)

        if not os.path.isdir(HOST_SYS_BLOCK):
            self._raw_system_devices = []
            return

        mounts = host_mounts()
        dev_to_uuid = {v: k for k, v in uuid_to_device().items()}

        devices: list[HostBlockDevice] = []
        for name in sorted(os.listdir(HOST_SYS_BLOCK)):
            if name.startswith(("loop", "ram", "dm-", "sr", "fd", "nbd")):
                continue
            block_dir = os.path.join(HOST_SYS_BLOCK, name)
            if not is_usb(block_dir):
                continue

            vendor = read(os.path.join(block_dir, "device", "vendor"))
            model = read(os.path.join(block_dir, "device", "model"))
            serial = (
                read(os.path.join(block_dir, "serial"))
                or read(os.path.join(block_dir, "device", "serial"))
            )
            if not serial:
                continue  # serials are assumed always present; skip otherwise

            partitions = [
                e for e in os.listdir(block_dir)
                if e.startswith(name) and e != name
                and os.path.isdir(os.path.join(block_dir, e))
            ]
            if not partitions:
                partitions = [name]  # whole-disk filesystem

            for part in partitions:
                part_dev = f"/dev/{part}"
                devices.append(HostBlockDevice(
                    device=part_dev,
                    vendor=vendor,
                    model=model,
                    serial=Serial(serial),
                    fs_uuid=dev_to_uuid.get(part_dev),
                    mountpoint=mounts.get(part_dev),
                ))

        # Changing the object ref is atomic in CPython; readers see one
        # complete list or the other.
        self._raw_system_devices = devices

    # ---- StorageClass discovery + matching -------------------------------- #

    def _refresh_storage_classes(self) -> None:
        self._storage_classes = [
            sc for sc in self._storage_v1.list_storage_class().items
            if sc.provisioner == PROVISIONER
        ]

    def _device_matches_sc(
        self, d: HostBlockDevice, sc: "client.V1StorageClass"
    ) -> bool:
        """True if `d` satisfies every match field set in `sc.parameters`.

        Supported parameters:
          vendor   — fnmatch glob against device's vendor
          model    — fnmatch glob against device's model
          serials  — comma-separated allowlist for serial number
        At least one must be set; an SC with no criteria matches nothing.
        """
        def parse_serials(blob: str | None) -> list[str]:
            if not blob:
                return []
            return [s.strip() for s in blob.split(",") if s.strip()]

        p = sc.parameters or {}
        has_criteria = False
        if "vendor" in p:
            if not fnmatch.fnmatchcase(d.vendor or "", p["vendor"]):
                return False
            has_criteria = True
        if "model" in p:
            if not fnmatch.fnmatchcase(d.model or "", p["model"]):
                return False
            has_criteria = True
        if "serials" in p:
            if d.serial not in parse_serials(p["serials"]):
                return False
            has_criteria = True
        return has_criteria

    # ---- registry (annotations on the StorageClass) ----------------------- #

    def _reconcile_devices(self) -> None:
        def annotation_key(serial: Serial) -> str:
            name = re.sub(r"[^A-Za-z0-9_.-]", "-", str(serial))
            name = re.sub(r"^[^A-Za-z0-9]+|[^A-Za-z0-9]+$", "", name) or "unknown"
            return f"{DEVICE_ANN_PREFIX}{name[:58]}"

        def patch_sc_annotation(sc_name: str, key: str, value: str | None) -> None:
            try:
                self._storage_v1.patch_storage_class(
                    sc_name,
                    {"metadata": {"annotations": {key: value}}},
                )
            except ApiException:
                log.exception("annotation patch on SC %s failed", sc_name)

        def find_existing(serial: Serial) -> RegisteredDevice | None:
            """Look up the most recent registration for `serial` across all SCs."""
            best: RegisteredDevice | None = None
            for sc in self._storage_classes:
                blob = (sc.metadata.annotations or {}).get(annotation_key(serial))
                if not blob:
                    continue
                try:
                    reg = RegisteredDevice.from_json(blob)
                except (json.JSONDecodeError, TypeError):
                    continue
                if best is None or reg.last_seen > best.last_seen:
                    best = reg
            return best

        now_iso = datetime.now(timezone.utc).isoformat()
        system_devices = self._raw_system_devices

        # Pair each local device with the SCs that match it.
        matched: dict[Serial, tuple[HostBlockDevice, list["client.V1StorageClass"]]] = {}
        for d in system_devices:
            scs = [sc for sc in self._storage_classes if self._device_matches_sc(d, sc)]
            if scs:
                matched[d.serial] = (d, scs)
            elif d.serial not in self._logged_unmatched:
                log.info(
                    "ignored device %s: no StorageClass matched (vendor=%r model=%r serial=%s)",
                    d.device, d.vendor, d.model, d.serial,
                )
                self._logged_unmatched.add(d.serial)

        # Insertion + migration detection. For each match, write the annotation
        # to every matching SC.
        for serial, (d, scs) in matched.items():
            prior = find_existing(serial)
            if prior is None:
                log.info("device inserted: serial=%s vendor=%r model=%r fs=%s",
                         serial, d.vendor, d.model, d.fs_uuid)
                self._emit_event(
                    "DeviceInserted",
                    f"Device serial {serial} ({d.vendor!r} {d.model!r}) inserted on {NODE_NAME}",
                )
            elif prior.node != NODE_NAME and prior.phase == "Present":
                log.warning("MIGRATION device serial=%s moved %s -> %s",
                            serial, prior.node, NODE_NAME)
                self._emit_event(
                    "DeviceMigrated",
                    f"Device serial {serial} moved from {prior.node} to {NODE_NAME}",
                    type_="Warning",
                )
                self._pending_migrations.add((serial, prior.node))

            reg = RegisteredDevice(
                serial=str(serial),
                vendor=d.vendor,
                model=d.model,
                fs_uuid=d.fs_uuid,
                node=NODE_NAME,
                mountpoint=d.mountpoint,
                first_seen=(prior.first_seen if prior else now_iso),
                last_seen=now_iso,
                phase="Present",
            )
            blob = reg.to_json()
            for sc in scs:
                patch_sc_annotation(sc.metadata.name, annotation_key(serial), blob)

        # Removal: annotations claiming THIS node for a serial we don't see
        # any more get marked Removed. We keep node= so a daemon on another
        # node can still detect "moved from X" on reappear.
        local_serials = {d.serial for d in system_devices}
        for sc in self._storage_classes:
            for key, val in (sc.metadata.annotations or {}).items():
                if not key.startswith(DEVICE_ANN_PREFIX):
                    continue
                try:
                    reg = RegisteredDevice.from_json(val)
                except (json.JSONDecodeError, TypeError):
                    continue
                if reg.node != NODE_NAME or reg.phase == "Removed":
                    continue
                if Serial(reg.serial) in local_serials:
                    continue
                reg.phase = "Removed"
                reg.last_seen = now_iso
                log.info("device removed: serial=%s vendor=%r model=%r (was on %s)",
                         reg.serial, reg.vendor, reg.model, NODE_NAME)
                self._emit_event(
                    "DeviceRemoved",
                    f"Device serial {reg.serial} removed from {NODE_NAME}",
                )
                patch_sc_annotation(sc.metadata.name, key, reg.to_json())

        # Refresh in-memory caches.
        self._available_devices = {s: m[0] for s, m in matched.items()}
        self._matched_scs = {s: m[1] for s, m in matched.items()}

        # Node labels: one class-<scname>=true per SC that has a ready device.
        local_sc_names = {
            sc.metadata.name
            for d, scs in matched.values()
            if d.mountpoint == DATA_MOUNTPOINT
            for sc in scs
        }
        self._update_node_labels(local_sc_names)

    def _update_node_labels(self, sc_names: set[str]) -> None:
        desired = {f"{CLASS_LABEL_PREFIX}{s}": "true" for s in sc_names}
        node = self._v1.read_node(NODE_NAME)
        current = {
            k: v for k, v in (node.metadata.labels or {}).items()
            if k.startswith(CLASS_LABEL_PREFIX)
        }
        patch: dict[str, str | None] = {}
        for k, v in desired.items():
            if current.get(k) != v:
                patch[k] = v
        for k in current:
            if k not in desired:
                patch[k] = None
        if patch:
            self._v1.patch_node(NODE_NAME, {"metadata": {"labels": patch}})
            log.info("node labels updated: %s", patch)

    # ---- format + mount --------------------------------------------------- #

    def _maybe_format_devices(self) -> None:
        def host_run(cmd: list[str]) -> subprocess.CompletedProcess:
            full = NSENTER + cmd
            log.info("host$ %s", " ".join(cmd))
            result = subprocess.run(full, check=False, capture_output=True, text=True)
            if result.returncode != 0:
                log.warning("host cmd failed (rc=%d): %s\nstderr=%s",
                            result.returncode, " ".join(cmd), result.stderr.strip())
            return result

        def host_format(device: str, fs_label: str | None) -> None:
            cmd = ["mkfs.ext4", "-F", "-q"]
            if fs_label:
                cmd += ["-L", fs_label[:16]]
            cmd.append(device)
            host_run(cmd)

        def host_mount(device: str, mountpoint: str) -> None:
            host_run(["mkdir", "-p", mountpoint])
            host_run(["mount", device, mountpoint])

        def data_mount_source() -> str | None:
            try:
                with open(HOST_MOUNTINFO) as f:
                    for line in f.read().splitlines():
                        parts = line.split()
                        if "-" not in parts:
                            continue
                        dash = parts.index("-")
                        if dash < 5 or len(parts) <= dash + 2:
                            continue
                        if parts[4] == DATA_MOUNTPOINT:
                            return parts[dash + 2]
            except OSError:
                pass
            return None

        def parse_bool(s: str | None) -> bool:
            return bool(s) and s.strip().lower() in ("true", "1", "yes", "on")

        def relookup_uuid(dev_path: str) -> str | None:
            for _ in range(10):
                if os.path.isdir(HOST_BY_UUID):
                    for u in os.listdir(HOST_BY_UUID):
                        try:
                            if os.path.realpath(os.path.join(HOST_BY_UUID, u)) == dev_path:
                                return u
                        except OSError:
                            pass
                time.sleep(0.5)
            return None

        for serial in sorted(self._available_devices):
            d = self._available_devices[serial]
            scs = self._matched_scs.get(serial, [])
            if not scs or d.mountpoint == DATA_MOUNTPOINT:
                continue
            sc = scs[0]  # first matching SC drives the one-shot format policy
            params = sc.parameters or {}

            if d.fs_uuid is None:
                if not parse_bool(params.get("autoFormat")):
                    log.warning("device %s has no FS and SC %s has autoFormat!=true; skipping",
                                d.device, sc.metadata.name)
                    continue
                log.warning("FORMAT %s (SC=%s)", d.device, sc.metadata.name)
                host_format(d.device, params.get("fsLabel"))
                d.fs_uuid = relookup_uuid(d.device)
                if d.fs_uuid is None:
                    log.warning("device %s formatted but no UUID appeared", d.device)
                    continue

            busy = data_mount_source()
            if busy and busy != d.device:
                log.warning("device %s wants /data but it is held by %s; skipping",
                            d.device, busy)
                continue
            if not busy:
                log.warning("MOUNT %s at %s", d.device, DATA_MOUNTPOINT)
                host_mount(d.device, DATA_MOUNTPOINT)
            d.mountpoint = DATA_MOUNTPOINT

    # ---- PV provisioning -------------------------------------------------- #

    def _add_pvs(self) -> None:
        def matches_our_provisioner(annotations: dict[str, str] | None) -> bool:
            a = annotations or {}
            return (a.get(PROVISIONER_ANN) == PROVISIONER
                    or a.get(PROVISIONER_ANN_NEW) == PROVISIONER)

        def node_affinity(node: str) -> "client.V1VolumeNodeAffinity":
            return client.V1VolumeNodeAffinity(
                required=client.V1NodeSelector(
                    node_selector_terms=[client.V1NodeSelectorTerm(
                        match_expressions=[client.V1NodeSelectorRequirement(
                            key="kubernetes.io/hostname", operator="In",
                            values=[node],
                        )]
                    )]
                )
            )

        def build_pv(pvc, sc, device: HostBlockDevice) -> "client.V1PersistentVolume":
            name = f"usb-{pvc.metadata.namespace}-{pvc.metadata.name}-{pvc.metadata.uid[:8]}"
            labels = {
                NODE_LABEL: NODE_NAME,
                CLASS_LABEL: sc.metadata.name,
                SERIAL_LABEL: str(device.serial),
            }
            return client.V1PersistentVolume(
                metadata=client.V1ObjectMeta(
                    name=name,
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
                    node_affinity=node_affinity(NODE_NAME),
                    claim_ref=client.V1ObjectReference(
                        api_version="v1", kind="PersistentVolumeClaim",
                        namespace=pvc.metadata.namespace, name=pvc.metadata.name,
                        uid=pvc.metadata.uid,
                        resource_version=pvc.metadata.resource_version,
                    ),
                ),
            )

        sc_by_name = {sc.metadata.name: sc for sc in self._storage_classes}

        for pvc in self._v1.list_persistent_volume_claim_for_all_namespaces().items:
            if pvc.spec.volume_name:
                continue
            if not matches_our_provisioner(pvc.metadata.annotations):
                continue
            sc = sc_by_name.get(pvc.spec.storage_class_name or "")
            if sc is None:
                continue
            selected = (pvc.metadata.annotations or {}).get(SELECTED_NODE_ANN)
            if selected and selected != NODE_NAME:
                continue

            candidates = [
                d for serial, d in self._available_devices.items()
                if d.mountpoint == DATA_MOUNTPOINT
                and sc in self._matched_scs.get(serial, [])
            ]
            if not candidates:
                continue
            device = candidates[0]

            target = os.path.join(HOST_DATA, pvc.metadata.name)
            os.makedirs(target, exist_ok=True)

            pv = build_pv(pvc, sc, device)
            try:
                self._v1.create_persistent_volume(pv)
            except ApiException as e:
                if e.status == 409:
                    continue
                raise

            log.info("provisioned PV %s for PVC %s/%s on %s (SC=%s serial=%s)",
                     pv.metadata.name, pvc.metadata.namespace, pvc.metadata.name,
                     NODE_NAME, sc.metadata.name, device.serial)
            self._emit_event(
                "Provisioned",
                f"PV {pv.metadata.name} provisioned on {NODE_NAME} "
                f"(SC {sc.metadata.name}, serial {device.serial})",
                regarding=client.V1ObjectReference(
                    api_version="v1", kind="PersistentVolumeClaim",
                    namespace=pvc.metadata.namespace, name=pvc.metadata.name,
                    uid=pvc.metadata.uid,
                ),
            )

    # ---- PV cleanup (reclaim=Delete) -------------------------------------- #

    def _rm_pvs(self) -> None:
        def pinned_to(pv, node: str) -> bool:
            aff = pv.spec.node_affinity
            if not aff or not aff.required:
                return False
            for t in aff.required.node_selector_terms or []:
                for e in t.match_expressions or []:
                    if e.key == "kubernetes.io/hostname" and node in (e.values or []):
                        return True
            return False

        for pv in self._v1.list_persistent_volume().items:
            if (pv.metadata.annotations or {}).get(PROVISIONED_BY_ANN) != PROVISIONER:
                continue
            if pv.spec.persistent_volume_reclaim_policy != "Delete":
                continue
            if pv.status.phase != "Released":
                continue
            if not pinned_to(pv, NODE_NAME):
                continue

            local = pv.spec.local
            if local and local.path:
                rel = os.path.relpath(local.path, "/data")
                target = os.path.join(HOST_DATA, rel)
                if (os.path.commonpath([os.path.abspath(target), HOST_DATA]) == HOST_DATA
                        and os.path.isdir(target)):
                    shutil.rmtree(target, ignore_errors=True)
                    log.info("removed data dir %s", target)
            try:
                self._v1.delete_persistent_volume(pv.metadata.name)
                log.info("deleted PV %s", pv.metadata.name)
            except ApiException as e:
                if e.status != 404:
                    raise

    # ---- migration -------------------------------------------------------- #

    def _maybe_migrate_pvs(self) -> None:
        """Process every migration queued by _reconcile_devices this tick."""
        def pinned_node(pv) -> str | None:
            aff = pv.spec.node_affinity
            if not aff or not aff.required:
                return None
            for t in aff.required.node_selector_terms or []:
                for e in t.match_expressions or []:
                    if e.key == "kubernetes.io/hostname":
                        return (e.values or [None])[0]
            return None

        def evict_pods(claim_ref) -> None:
            if not claim_ref:
                return
            try:
                pods = self._v1.list_namespaced_pod(claim_ref.namespace).items
            except ApiException:
                return
            for pod in pods:
                for vol in pod.spec.volumes or []:
                    cr = vol.persistent_volume_claim
                    if cr and cr.claim_name == claim_ref.name:
                        log.warning("MIGRATION evicting pod %s/%s (uses PVC %s)",
                                    pod.metadata.namespace, pod.metadata.name, claim_ref.name)
                        try:
                            self._v1.delete_namespaced_pod(
                                pod.metadata.name, pod.metadata.namespace,
                                grace_period_seconds=0,
                            )
                        except ApiException as e:
                            if e.status != 404:
                                log.warning("could not delete pod: %s", e)
                        break

        def node_affinity(node: str) -> "client.V1VolumeNodeAffinity":
            return client.V1VolumeNodeAffinity(
                required=client.V1NodeSelector(
                    node_selector_terms=[client.V1NodeSelectorTerm(
                        match_expressions=[client.V1NodeSelectorRequirement(
                            key="kubernetes.io/hostname", operator="In",
                            values=[node],
                        )]
                    )]
                )
            )

        def recreate_pv(pv, new_node: str) -> bool:
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
                    node_affinity=node_affinity(new_node),
                    claim_ref=client.V1ObjectReference(
                        api_version="v1", kind="PersistentVolumeClaim",
                        namespace=pv.spec.claim_ref.namespace,
                        name=pv.spec.claim_ref.name,
                        uid=pv.spec.claim_ref.uid,
                    ) if pv.spec.claim_ref else None,
                ),
            )
            try:
                self._v1.patch_persistent_volume(name, {"metadata": {"finalizers": []}})
            except ApiException:
                pass
            try:
                self._v1.delete_persistent_volume(name)
            except ApiException as e:
                if e.status != 404:
                    log.warning("MIGRATION delete PV %s failed: %s", name, e)
                    return False
            for _ in range(30):
                try:
                    self._v1.read_persistent_volume(name)
                    time.sleep(1)
                except ApiException as e:
                    if e.status == 404:
                        break
            else:
                log.warning("PV %s still present after 30s, skipping recreate", name)
                return False
            try:
                self._v1.create_persistent_volume(replacement)
            except ApiException as e:
                log.warning("MIGRATION recreate PV %s failed: %s", name, e)
                return False
            return True

        while self._pending_migrations:
            serial, old_node = self._pending_migrations.pop()
            pvs = self._v1.list_persistent_volume(
                label_selector=f"{SERIAL_LABEL}={serial}",
            ).items
            targets = [pv for pv in pvs if pinned_node(pv) == old_node]
            if not targets:
                continue

            log.warning("MIGRATION serial=%s: %s -> %s, %d PV(s)",
                        serial, old_node, NODE_NAME, len(targets))

            for pv in targets:
                evict_pods(pv.spec.claim_ref)
            for pv in targets:
                if recreate_pv(pv, NODE_NAME):
                    log.warning("MIGRATION recreated PV %s pinned to %s",
                                pv.metadata.name, NODE_NAME)
                    self._emit_event(
                        "PVMigrated",
                        f"PV {pv.metadata.name} migrated from {old_node} to {NODE_NAME} "
                        f"(serial {serial})",
                        type_="Warning",
                        regarding=client.V1ObjectReference(
                            api_version="v1", kind="PersistentVolume", name=pv.metadata.name,
                        ),
                    )

    # ---- events + health -------------------------------------------------- #

    def _emit_event(
        self,
        reason: str,
        message: str,
        regarding: "client.V1ObjectReference | None" = None,
        type_: str = "Normal",
    ) -> None:
        now = datetime.now(timezone.utc)
        name = f"usb-storage.{reason.lower()}.{int(now.timestamp())}.{os.urandom(2).hex()}"
        involved = regarding or client.V1ObjectReference(
            api_version="v1", kind="Node", name=NODE_NAME,
        )
        event = client.CoreV1Event(
            metadata=client.V1ObjectMeta(name=name, namespace=PROVISIONER_NS),
            type=type_, reason=reason, message=message,
            source=client.V1EventSource(component="usb-storage-provisioner", host=NODE_NAME),
            first_timestamp=now, last_timestamp=now,
            involved_object=involved,
        )
        try:
            self._v1.create_namespaced_event(PROVISIONER_NS, event)
        except ApiException:
            log.exception("emit event %s failed", reason)

    def _start_health_server(self) -> None:
        provisioner = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                if self.path != "/health":
                    self.send_response(404); self.end_headers(); return
                fresh = (time.time() - provisioner._last_reconcile_ok) < HEALTH_STALE_AFTER
                self.send_response(200 if fresh else 503)
                self.send_header("Content-Type", "text/plain"); self.end_headers()
                self.wfile.write(b"ok\n" if fresh else b"stale\n")

            def log_message(self, *_args, **_kwargs):
                pass

        server = HTTPServer(("0.0.0.0", HEALTH_PORT), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        log.info("health server listening on :%d", HEALTH_PORT)

    # ---- main loop -------------------------------------------------------- #

    def start(self) -> None:
        log.info("usb-storage provisioner starting on node %s", NODE_NAME)
        self._start_health_server()
        while True:
            try:
                self._refresh_system_devices()
                self._refresh_storage_classes()
                self._reconcile_devices()
                self._maybe_format_devices()
                self._add_pvs()
                self._rm_pvs()
                self._maybe_migrate_pvs()
                self._last_reconcile_ok = time.time()
            except Exception:
                log.exception("reconcile error")
            time.sleep(POLL_INTERVAL)


def main() -> None:
    UsbStorageProvisioner().start()


if __name__ == "__main__":
    main()
