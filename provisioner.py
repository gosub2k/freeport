#!/usr/bin/env python3

import logging
from dataclasses import dataclass
from abc import ABC,  abstractmethod
from enum import Enum

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("freeport")


class Serial(str):
    """Typed alias for USB serial numbers."""

    pass



class BlockDeviceType(Enum):

    # usb, mmc, etc ...
    # right now only usb is supported
    # maps strings to types also: usb -> BlockDeviceType.USB


@dataclass
class HostBlockDevice:
    """A USB block device as seen by the kernel on this node."""

    device: str
    vendor: str
    type: BlockDeviceType
    model: str
    serial: Serial
    fs_uuid: str | None
    mountpoint: str | None

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

    def __call__(self, dev: HostBlockDevice) -> bool:
        """ does filter match? """
        pass


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
    def _remove_device_from_known_devices(self, HostBlockDevice):
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

    def __init__(self, filter: DeviceFilter) -> None:
        # super().__init__(filter)
       self._known_devices: list[HostBlockDevice] = list()
       self._filter = filter

    # TODO: implement
    #
    
