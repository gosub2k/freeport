#!/usr/bin/env python3

import logging
import os
import re
import subprocess
import time

from abstract_manager import (
    DeviceFilter,
    HostBlockDevice,
    Serial,
    USBDeviceManager,
    log,
)

# Utils

# nsenter into the host mount + PID namespaces via PID 1 (requires hostPID:
# true and privileged: true in the DaemonSet spec). The mount call then takes
# effect on the host, so hostPath PV subdirs are visible to other pods.
_NSENTER = ["nsenter", "--target", "1", "--mount", "--"]


def get_df(dev_path) -> int:
    dev_path = re.sub("/mnt", InMemoryUSBDeviceManager.HOST_MOUNT, dev_path)
    try:
        st = os.statvfs(dev_path)
        df = st.f_bavail * st.f_frsize  # available to non-root
        log.info("get_df(%s) = %d", dev_path, df)
        return df
    except Exception as e:
        log.error(e)
        return 0


def mount(dev_path: str, serial: str) -> str | None:
    # mount the device onto the host so that we can later create sub dirs that can be
    # used in local or hostPath sources for PVs.
    mountpoint = f"/mnt/k8s-freeport-{serial}"
    try:
        with open("/proc/1/mounts") as f:
            for line in f:
                parts = line.split()
                if len(parts) >= 2 and parts[0] == dev_path:
                    return parts[1]  # already mounted, return existing mountpoint
    except OSError:
        pass

    subprocess.run(_NSENTER + ["mkdir", "-p", mountpoint], check=True)
    result = subprocess.run(
        _NSENTER + ["mount", dev_path, mountpoint], capture_output=True, text=True
    )
    if result.returncode != 0:
        log.error(
            "mount %s -> %s failed: %s", dev_path, mountpoint, result.stderr.strip()
        )
        return None
    log.info("mounted %s at %s", dev_path, mountpoint)
    return mountpoint


class InMemoryUSBDeviceManager(USBDeviceManager):

    HOST_MOUNT = "/mnt"
    USB_DEVDIR = "/dev/disk/by-id"
    SYS_CLASS_BLOCK = "/sys/class/block"
    # usb-...-partN only identifies a usb partition link and its number;
    # vendor / model / serial come from sysfs, not this name (the by-id name
    # mangles spaces to underscores and may carry an empty vendor field).
    USB_REGEXP = (
        r"usb-.+-part(\d+)"  # e.g. usb-_USB_DISK_2.0_900053B3E984FA19-0:0-part1
    )

    def __init__(self, filter: DeviceFilter) -> None:
        self._filter = filter
        self._known_devices: list[HostBlockDevice] = list()
        # what the "kernel" sees; injectable for tests, mutate to simulate
        # insertion / removal between reconcile() calls.
        self._system_devices: list[HostBlockDevice] = list()

    @staticmethod
    def _read_sys_attr(path: str) -> str:
        try:
            with open(path) as f:
                return f.read().strip()
        except OSError:
            return ""

    def _usb_node_for(self, device: str) -> str | None:
        """Walk sysfs up from a block device node (e.g. /dev/sda1) to the
        USB device directory that carries manufacturer/product/serial.

        /sys/class/block/sda1 -> .../usb1/1-3/.../block/sda/sda1; the USB
        node is the nearest ancestor holding a `serial` file.
        """
        base = os.path.basename(device)
        d = os.path.realpath(os.path.join(self.SYS_CLASS_BLOCK, base))
        while os.path.isdir(d):
            if os.path.exists(os.path.join(d, "serial")):
                return d
            parent = os.path.dirname(d)
            if parent == d:
                break
            d = parent
        return None

    def _list_usb_devices_on_system(self) -> list[HostBlockDevice]:
        # discover usb partitions via /dev/disk/by-id, then read the real
        # manufacturer / product / serial from the device's sysfs node.
        devs: list[HostBlockDevice] = []
        pattern = re.compile(self.USB_REGEXP)
        try:
            entries = os.listdir(self.USB_DEVDIR)
        except OSError:
            log.warning("cannot read %s", self.USB_DEVDIR)
            return devs

        for name in sorted(entries):
            m = pattern.fullmatch(name)
            if not m:
                continue  # not a usb partition link
            partition = int(m.group(1))
            device = os.path.realpath(os.path.join(self.USB_DEVDIR, name))

            usb_node = self._usb_node_for(device)
            if usb_node is None:
                log.warning("no usb sysfs node for %s; skipping", device)
                continue
            serial = Serial(self._read_sys_attr(os.path.join(usb_node, "serial")))
            mountpoint = mount(device, serial)
            dev = HostBlockDevice(
                manufacturer=self._read_sys_attr(
                    os.path.join(usb_node, "manufacturer")
                ),
                type=BlockDeviceType.USB,
                model=self._read_sys_attr(os.path.join(usb_node, "product")),
                serial=serial,
                partition=partition,
                free=get_df(mountpoint),
                mountpoint=mountpoint,
            )

            # apply the filter; log matches with a leading (*)
            if self._filter(dev):
                log.debug("(*) %s", dev)
                devs.append(dev)
            else:
                log.debug("(X) %s", dev)

        return devs

    def _add_device_to_known_devices(self, device: HostBlockDevice) -> bool:
        if device in self._known_devices:
            return False
        self._known_devices.append(device)
        log.info(f"device added: {device}")
        return True

    def _get_known_devices(self) -> list[HostBlockDevice]:
        return list(self._known_devices)

    def _remove_device_from_known_devices(self, device: HostBlockDevice) -> bool:
        if device not in self._known_devices:
            return False
        self._known_devices.remove(device)
        return True


def sanity_check_loop():
    filter = DeviceFilter(*{"type": "usb,nvme"})
    mngr = InMemoryUSBDeviceManager(filter)
    while True:
        mngr.reconcile()
        time.sleep(1)


if __name__ == "__main__":
    sanity_check_loop()
