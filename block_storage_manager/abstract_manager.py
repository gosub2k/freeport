#!/usr/bin/env python3

import logging
import re
from abc import ABC, abstractmethod
from dataclasses import dataclass
from enum import Enum

class _ScFormatter(logging.Formatter):
    def format(self, record):
        if not hasattr(record, "sc"):
            record.sc = "-"
        return super().format(record)

_handler = logging.StreamHandler()
_handler.setFormatter(_ScFormatter("%(asctime)s %(levelname)s [%(sc)s] %(message)s"))
logging.getLogger().addHandler(_handler)
logging.getLogger().setLevel(logging.INFO)
log = logging.getLogger("freeport")


# Stuff

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
    free: int
    mountpoint: str

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

    @abstractmethod
    def _refresh_device_info(self, device: HostBlockDevice) -> bool:
        pass

    def reconcile(self):
        # REVISIT: cleaner logic for multi node setup?
        system_devices = self._list_usb_devices_on_system()
        known_devices = self._get_known_devices()

        for sd in system_devices:
            if sd not in known_devices:
                self._add_device_to_known_devices(sd)
            else:
                self._refresh_device_info(sd)

        known_devices = self._get_known_devices()
        for kd in known_devices:
            if (
                kd not in system_devices
            ):  # should be using the HostBlockDevice.__eq__() method
                self._remove_device_from_known_devices(kd)
