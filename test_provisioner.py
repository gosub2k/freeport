#!/usr/bin/env python3
"""Standalone tests for the USB device discovery path.

Runs without a cluster: builds a fake /dev/disk/by-id tree under
/tmp/sys-test/ and points the manager's class-level path constants at it.

    python3 test_provisioner.py            # or: python3 -m unittest -v
"""

import logging
import os
import shutil
import unittest
from unittest import mock

from provisioner import (
    BlockDeviceType,
    DeviceFilter,
    InMemoryUSBDeviceManager,
)

logging.disable(logging.CRITICAL)  # keep test output quiet

TEST_ROOT = "/tmp/sys-test"
BY_ID = os.path.join(TEST_ROOT, "disk", "by-id")
DEVNODES = os.path.join(TEST_ROOT, "dev")

# Symlink name (as found in by-id) -> real device node it resolves to.
# Mirrors the kernel convention: usb-<vendor>_<model>_<serial>-<lun>-partN.
LAYOUT = {
    "usb-_USB_DISK_2.0_900053B3E984FA19-0:0-part1": "sda1",
    "usb-_USB_DISK_2.0_900053B3E984FA19-0:0-part2": "sda2",
    "usb-Kingston_DataTraveler_3.0_AABBCC-0:0-part1": "sdb1",
    # whole-disk usb link (no -partN): must be ignored
    "usb-_USB_DISK_2.0_900053B3E984FA19-0:0": "sda",
    # non-usb device: must be ignored
    "ata-Samsung_SSD_860_S3Z9-part1": "sdc1",
}

MATCH_ALL = DeviceFilter("", "", "", "", "")


def build_tree(layout: dict[str, str]) -> None:
    """(Re)create /tmp/sys-test with the given by-id symlinks."""
    shutil.rmtree(TEST_ROOT, ignore_errors=True)
    os.makedirs(BY_ID)
    os.makedirs(DEVNODES)
    for link_name, target in layout.items():
        node = os.path.join(DEVNODES, target)
        open(node, "w").close()  # placeholder device node
        # relative symlink, exactly like the real by-id entries
        rel = os.path.relpath(node, BY_ID)
        os.symlink(rel, os.path.join(BY_ID, link_name))


@mock.patch.object(InMemoryUSBDeviceManager, "USB_DEVDIR", BY_ID)
class ListUsbDevicesTest(unittest.TestCase):
    def setUp(self):
        build_tree(LAYOUT)

    def tearDown(self):
        shutil.rmtree(TEST_ROOT, ignore_errors=True)

    def _serials(self, devs):
        return {d.serial for d in devs}

    def test_lists_only_usb_partitions(self):
        mgr = InMemoryUSBDeviceManager(MATCH_ALL)
        devs = mgr._list_usb_devices_on_system()

        # two sda partitions + one sdb partition; whole-disk and ata ignored
        self.assertEqual(len(devs), 3)
        self.assertTrue(all(d.type is BlockDeviceType.USB for d in devs))

    def test_fields_parsed_from_by_id(self):
        mgr = InMemoryUSBDeviceManager(MATCH_ALL)
        by_dev = {d.device: d for d in mgr._list_usb_devices_on_system()}

        sda1 = by_dev[os.path.join(DEVNODES, "sda1")]
        self.assertEqual(sda1.serial, "_USB_DISK_2.0_900053B3E984FA19-0:0")
        self.assertEqual(sda1.partition, 1)

        sda2 = by_dev[os.path.join(DEVNODES, "sda2")]
        self.assertEqual(sda2.partition, 2)
        # both partitions share a serial -> compare equal (serial-only __eq__)
        self.assertEqual(sda1, sda2)

    def test_symlinks_resolve_to_real_nodes(self):
        mgr = InMemoryUSBDeviceManager(MATCH_ALL)
        for d in mgr._list_usb_devices_on_system():
            self.assertTrue(d.device.startswith(DEVNODES))
            self.assertTrue(os.path.exists(d.device))

    def test_filter_by_serial(self):
        mgr = InMemoryUSBDeviceManager(DeviceFilter("", "", "", "Kingston.*", ""))
        devs = mgr._list_usb_devices_on_system()
        self.assertEqual(len(devs), 1)
        self.assertEqual(devs[0].serial, "Kingston_DataTraveler_3.0_AABBCC-0:0")

    def test_filter_excludes_everything(self):
        mgr = InMemoryUSBDeviceManager(DeviceFilter("", "", "", "nope-no-match", ""))
        self.assertEqual(mgr._list_usb_devices_on_system(), [])

    def test_missing_devdir_returns_empty(self):
        shutil.rmtree(TEST_ROOT, ignore_errors=True)
        mgr = InMemoryUSBDeviceManager(MATCH_ALL)
        self.assertEqual(mgr._list_usb_devices_on_system(), [])

    def test_reconcile_adds_then_removes(self):
        mgr = InMemoryUSBDeviceManager(MATCH_ALL)
        mgr.reconcile()
        # 3 partitions but two share the sda serial -> 2 unique known devices
        self.assertEqual(len(mgr._get_known_devices()), 2)

        # unplug the Kingston stick and reconcile again
        os.remove(os.path.join(BY_ID, "usb-Kingston_DataTraveler_3.0_AABBCC-0:0-part1"))
        mgr.reconcile()

        known_serials = {d.serial for d in mgr._get_known_devices()}
        self.assertEqual(known_serials, {"_USB_DISK_2.0_900053B3E984FA19-0:0"})


if __name__ == "__main__":
    unittest.main(verbosity=2)
