#!/usr/bin/env python3

import logging
import os
import re
import time
from abc import ABC, abstractmethod
from dataclasses import dataclass
from enum import Enum

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("freeport")


"""Typed alias for USB serial numbers."""


class Serial(str):
    pass


class BlockDeviceType(Enum):
    """Kind of host block device.
    usb, mmc, etc ... right now only usb is supported.
    """

    # Enum values
    USB = "usb"
    NVME = "nvme"

    # lookup - ie BlockDeviceType("usb") = BlockDeviceType.USB
    @classmethod
    def _missing_(cls, value: str):
        for member in cls:
            if member.value == value:
                return member
        raise ValueError()


@dataclass
class HostBlockDevice:
    """A USB block device as seen by the kernel on this node."""

    manufacturer: str
    type: BlockDeviceType
    model: str
    serial: Serial
    partition: int

    def __str__(self):
        return f'serial: {self.serial}, manufacterer: "{self.manufacturer}", type: {self.type}, model: "{self.model}" partition: {self.partition or "??"}'

    def __eq__(self, other):
        # REVISIT: could warn if serial is the same but others are different
        # unknown if serials are always present or truely unique
        return self.serial == other.serial


@dataclass
class DeviceFilter:
    """A filter that when called determines if the device matches regexps for device, vendor, model, and serial"""

    # expressions are a cannonical-style expression
    # we match ie "SAMSUNG.*" or "FIJI,SAMSUNG,FOO"
    manufacterer: str = ""
    model: str = ""
    serial: str = ""
    type: str = ""  # validate against the enum value

    @staticmethod
    def _match(expr: str, value: str) -> bool:
        """Match `value` against one canonical expression.

        An expression is a comma-separated list of regex alternatives, e.g.
        "SAMSUNG.*" or "FIJI,SAMSUNG,FOO". The value must fully match at
        least one alternative. An empty expression matches anything (the
        field is not used to discriminate).
        """
        if not expr:
            return True
        if not value:
            return False
        for alt in expr.split(","):
            alt = alt.strip()
            if alt and re.fullmatch(alt, value):
                return True
        return False

    def __call__(self, dev: HostBlockDevice) -> bool:
        """does filter match? all set fields must pass"""
        return (
            self._match(self.manufacterer, dev.manufacturer)
            and self._match(self.model, dev.model)
            and self._match(self.serial, dev.serial)
            and self._match(self.type, dev.type.value)
        )


class USBDeviceManager(ABC):

    @abstractmethod
    def __init__(self, filter: DeviceFilter):
        self._filter = filter

    @abstractmethod
    def _list_usb_devices_on_system(self) -> list[HostBlockDevice]:
        # grabs the block devices on the system
        # we can
        pass

    @abstractmethod
    def _add_device_to_known_devices(self, device: HostBlockDevice) -> bool:
        pass

    @abstractmethod
    def _get_known_devices(self) -> list[HostBlockDevice]:
        pass

    @abstractmethod
    def _remove_device_from_known_devices(self, device: HostBlockDevice) -> bool:
        pass

    def reconcile(self):
        # REVISIT: cleaner logic for multi node setup?
        system_devices = self._list_usb_devices_on_system()
        known_devices = self._get_known_devices()

        for sd in system_devices:
            if sd not in known_devices:
                self._add_device_to_known_devices(sd)

        known_devices = self._get_known_devices()
        for kd in known_devices:
            if (
                kd not in system_devices
            ):  # should be using the HostBlockDevice.__eq__() method
                self._remove_device_from_known_devices(kd)


class InMemoryUSBDeviceManager(USBDeviceManager):

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

            dev = HostBlockDevice(
                manufacturer=self._read_sys_attr(
                    os.path.join(usb_node, "manufacturer")
                ),
                type=BlockDeviceType.USB,
                model=self._read_sys_attr(os.path.join(usb_node, "product")),
                serial=Serial(self._read_sys_attr(os.path.join(usb_node, "serial"))),
                partition=partition,
            )

            # apply the filter; log matches with a leading (*)
            if self._filter(dev):
                log.info("(*) %s", dev)
                devs.append(dev)
            else:
                log.info("    %s", dev)

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
