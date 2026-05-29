#!/usr/bin/env python3

import logging
import re
from dataclasses import dataclass
from abc import ABC,  abstractmethod
from enum import Enum

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("freeport")


class Serial(str):
    """Typed alias for USB serial numbers."""

    pass



class BlockDeviceType(Enum):
    """Kind of host block device.

    usb, mmc, etc ... right now only usb is supported.
    """

    USB = "usb"

    @classmethod
    def _missing_(cls, value: str):
        for member in cls:
            if member.value == value:
                return member
        raise ValueError()


@dataclass
class HostBlockDevice:
    """A USB block device as seen by the kernel on this node."""

    device: str
    vendor: str
    type: BlockDeviceType
    model: str
    serial: Serial
    partition: int

    def __str__(self):
        return f'serial: {self.serial}, vendor: {self.vendor or "_"}, type: {self.type}, model: {self.model or ""} partitions: {self.partition or "??"}'
    def __eq__(self, other):
        # REVISIT: could warn if serial is the same but others are different
        # unknown if serials are always present or truely unique
        return self.serial == other.serial


@dataclass
class DeviceFilter:
    """A filter that when called determines if the device matches regexps for device, vendor, model, and serial"""

    # expressions are a cannonical-style expression
    # we match ie "SAMSUNG.*" or "FIJI,SAMSUNG,FOO"
    device_exp: str
    vendor_exp: str
    model_exp: str
    serial_exp: str
    type_exp: str  # should validate against mapping in BlockDeviceType - LATER


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
        """ does filter match? all set fields must pass """
        return (
            self._match(self.device_exp, dev.device)
            and self._match(self.vendor_exp, dev.vendor)
            and self._match(self.model_exp, dev.model)
            and self._match(self.serial_exp, dev.serial)
            and self._match(self.type_exp, dev.type.value)
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
    def _add_device_to_known_devices(self, device: HostBlockDevice) -> None:
        pass

    @abstractmethod
    def _get_known_devices(self) -> list[HostBlockDevice]:
        pass

    @abstractmethod
    def _remove_device_from_known_devices(self, device: HostBlockDevice) -> bool:
        pass
            

    def reconcile(self):
        system_devices = self._list_usb_devices_on_system()
        known_devices = self._get_known_devices()

        for sd in system_devices:
            if sd not in known_devices:
                self._add_device_to_known_devices(sd)

        known_devices = self._get_known_devices()
        for kd in known_devices:
            if kd not in system_devices:  # should be using the HostBlockDevice.__eq__() method
                self._remove_device_from_known_devices(kd)



class InMemoryUSBDeviceManager(USBDeviceManager):

    USB_DEVDIR="/dev/disk/by-id"
    USB_REGEXP=r"usb-(.*)-part(\d+)" # /dev/disk/by-id/usb-_USB_DISK_2.0_900053B3E984FA19-0:0-part1 
    def __init__(
        self, filter: DeviceFilter
    ) -> None:
        self._filter = filter
        self._known_devices: list[HostBlockDevice] = list()
        # what the "kernel" sees; injectable for tests, mutate to simulate
        # insertion / removal between reconcile() calls.
        self._system_devices: list[HostBlockDevice] = list()

    def _list_usb_devices_on_system(self) -> list[HostBlockDevice]:
        # TODO: this should list all usb devices by looking at
        # /dev/disk/by-id/usb- *part for example
        devs: list[HostBlockDevice] = list[HostBlockDevice]
        import os
        for f in os.listdir()

        # use os.path.realath,  maybe .startwith or .contains to check usb devices
        #
        # appy the filter
        #
        # log which match with (*)
        # (*) device ...
        #     device


    def _add_device_to_known_devices(self, device: HostBlockDevice) -> None:
        self._known_devices.append(device)
        log.info("device added: serial=%s model=%r", device.serial, device.model)

    def _get_known_devices(self) -> list[HostBlockDevice]:
        return list(self._known_devices)

    def _remove_device_from_known_devices(self, device: HostBlockDevice) -> bool:
        # TODO this should simply remove the device frm the ist (true if found)

