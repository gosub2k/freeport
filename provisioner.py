import fnmatch
import os
from dataclasses import dataclass
from threading import Lock


class Serial(str):
    pass


HOST_SYS_BLOCK = "/host/sys/block"
HOST_SYS_BLOCK = "/host/sys/block"
HOST_MOUNTINFO = "/host/proc/1/mountinfo"
HOST_BY_UUID = "/host/dev/disk/by-uuid"


@dataclass
class HostBlockDevice:
    device: str
    vendor: str
    model: str
    serial: Serial
    fs_uuid: str | None
    mountpoint: str | None


class UsbStorageProvisioner:

    def __init__(self):
        self._raw_system_devices: list[HostBlockDevice] = list()
        self._available_devices: dict[Serial, HostBlockDevice] = dict()

    def _refresh_system_devices(self):
        def _read(path) -> str:
            try:
                with open(path) as f:
                    return f.read().strip()
            except OSError:
                return ""

        def _host_mounts() -> dict[str,str]:
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

        def _uuid_to_device() -> dict[str,str]:
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

        devices = []
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
            serial = _read(os.path.join(block_dir, "serial")) or _read(
                os.path.join(block_dir, "device", "serial")
            )

            partitions = [
                e
                for e in os.listdir(block_dir)
                if e.startswith(name)
                and e != name
                and os.path.isdir(os.path.join(block_dir, e))
            ]
            if not partitions:
                partitions = [name]  # whole-disk filesystem

            for part in partitions:
                part_dev = f"/dev/{part}"
                devices.append(
                    HostBlockDevice(
                        device=part_dev,
                        vendor=vendor,
                        model=model,
                        serial=Serial(serial),
                        fs_uuid=dev_to_uuid.get(part_dev),
                        mountpoint=mounts.get(part_dev),
                    ),
                )

        # changing object ref is atomic
        self._raw_system_devices = devices

    def _reconcile_devices(self):
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


        system_devices = self._raw_system_devices  # atomic assignment


        # add devices
        for sys_dev in system_devices:
            if self._available_devices.get(sys_dev.serial) is None and device_matches_sc(sys_dev, sc):
                # TODO: emit event
                self._available_devices[sys_dev.serial] = sys_dev

        # remove devices
        # TODO: lock/atomic swap
        serials_on_system = [ d.serial for d in system_devices ]
        for available_dev in self._available_devices.items():
            if avalable_dev.serial not in serials_on_system:
                del self._available_devices[available_dev.serial]

    def _

    def _add_pvs():
        pass

    def start():
        self._refresh_system_devices()
        self._add_devices()
        self._rm_devics()
        self._add_pvs()
        self._rm_pvs()


def main():
    p = UsbStorageProvisioner()
    p.start()


if __name__ == "__main__":

    main()
