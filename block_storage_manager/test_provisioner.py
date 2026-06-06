#!/usr/bin/env python3
"""Standalone tests for the USB device discovery path.

Runs without a cluster: builds a fake sysfs + /dev/disk/by-id tree under
/tmp/sys-test/ and points the manager's class-level path constants at it.
The tree mirrors the real kernel layout, e.g.

    /dev/disk/by-id/usb-..._SERIAL-0:0-part1 -> /tmp/sys-test/dev/sda1
    /sys/class/block/sda1 -> .../usb1/1-3/.../block/sda/sda1
    .../usb1/1-3/{manufacturer,product,serial}

so vendor/model/serial are read from sysfs, exactly like production.

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
DEV = os.path.join(TEST_ROOT, "dev")
BY_ID = os.path.join(DEV, "disk", "by-id")
SYS_DEVICES = os.path.join(TEST_ROOT, "sys", "devices")
SYS_CLASS_BLOCK = os.path.join(TEST_ROOT, "sys", "class", "block")

# Faithful model of two USB sticks. `product` deliberately contains spaces
# (which the by-id name would mangle to underscores) to prove we read sysfs.
DISKS = {
    "sda": {
        "usb_node": "pci/usb1/1-3",  # ancestor dir holding the attr files
        "block_chain": "1-3:1.0/host0/target0:0:0/0:0:0:0/block/sda",
        "manufacturer": "        ",  # whitespace-padded, must be stripped -> ""
        "product": "USB DISK 2.0",
        "serial": "900053B3E984FA19",
        "by_id": "usb-_USB_DISK_2.0_900053B3E984FA19-0:0-part{p}",
        "partitions": [1, 2],
    },
    "sdb": {
        "usb_node": "pci/usb1/1-4/1-4.1",
        "block_chain": "1-4.1:1.0/host2/target2:0:0/2:0:0:0/block/sdb",
        "manufacturer": "SanDisk",
        "product": "U3 Cruzer Micro",
        "serial": "0000187DA57212DB",
        "by_id": "usb-SanDisk_U3_Cruzer_Micro_0000187DA57212DB-0:0-part{p}",
        "partitions": [1],
    },
}

MATCH_ALL = DeviceFilter()


def _write(path: str, content: str) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(content)


def _symlink(target: str, link: str) -> None:
    os.makedirs(os.path.dirname(link), exist_ok=True)
    os.symlink(os.path.relpath(target, os.path.dirname(link)), link)


def build_tree() -> None:
    shutil.rmtree(TEST_ROOT, ignore_errors=True)
    for disk, spec in DISKS.items():
        usb_node = os.path.join(SYS_DEVICES, spec["usb_node"])
        _write(os.path.join(usb_node, "manufacturer"), spec["manufacturer"])
        _write(os.path.join(usb_node, "product"), spec["product"])
        _write(os.path.join(usb_node, "serial"), spec["serial"])

        block_leaf = os.path.join(usb_node, spec["block_chain"])
        for p in spec["partitions"]:
            part = f"{disk}{p}"  # e.g. sda1
            part_sysdir = os.path.join(block_leaf, part)
            os.makedirs(part_sysdir, exist_ok=True)
            # /sys/class/block/sda1 -> sysfs partition dir
            _symlink(part_sysdir, os.path.join(SYS_CLASS_BLOCK, part))
            # /dev node + by-id partition link
            devnode = os.path.join(DEV, part)
            _write(devnode, "")
            _symlink(devnode, os.path.join(BY_ID, spec["by_id"].format(p=p)))

    # --- decoys that must be ignored ---
    # whole-disk usb link (no -partN)
    _write(os.path.join(DEV, "sda"), "")
    _symlink(
        os.path.join(DEV, "sda"),
        os.path.join(BY_ID, "usb-_USB_DISK_2.0_900053B3E984FA19-0:0"),
    )
    # non-usb device
    _write(os.path.join(DEV, "sdc1"), "")
    _symlink(
        os.path.join(DEV, "sdc1"),
        os.path.join(BY_ID, "ata-Samsung_SSD_860_S3Z9-part1"),
    )


def _fake_mount(dev_path: str, serial: str):
    return f"/mnt/k8s-freeport-{serial}"


@mock.patch("provisioner.mount", side_effect=_fake_mount)
@mock.patch.object(InMemoryUSBDeviceManager, "USB_DEVDIR", BY_ID)
@mock.patch.object(InMemoryUSBDeviceManager, "SYS_CLASS_BLOCK", SYS_CLASS_BLOCK)
class ListUsbDevicesTest(unittest.TestCase):
    def setUp(self, *_):
        build_tree()

    def tearDown(self, *_):
        shutil.rmtree(TEST_ROOT, ignore_errors=True)

    def _by_serial(self, devs):
        return {d.serial: d for d in devs}

    def test_lists_only_usb_partitions(self, *_):
        devs = InMemoryUSBDeviceManager(MATCH_ALL)._list_usb_devices_on_system()
        # sda1 + sda2 + sdb1; whole-disk and ata links ignored
        self.assertEqual(len(devs), 3)
        self.assertTrue(all(d.type is BlockDeviceType.USB for d in devs))

    def test_vendor_and_model_read_from_sysfs(self, *_):
        # the FIX: vendor=manufacturer, model=product, taken from sysfs.
        # product keeps its spaces ("USB DISK 2.0") — impossible if we'd
        # parsed the underscore-joined by-id name.
        by_serial = self._by_serial(
            InMemoryUSBDeviceManager(MATCH_ALL)._list_usb_devices_on_system()
        )

        sda = by_serial["900053B3E984FA19"]
        self.assertEqual(sda.manufacturer, "")  # blank manufacturer, stripped
        self.assertEqual(sda.model, "USB DISK 2.0")

        sdb = by_serial["0000187DA57212DB"]
        self.assertEqual(sdb.manufacturer, "SanDisk")
        self.assertEqual(sdb.model, "U3 Cruzer Micro")

    def test_serial_read_from_sysfs(self, *_):
        # the FIX: serial is the sysfs `serial`, not a chunk of the by-id name.
        serials = {
            d.serial
            for d in InMemoryUSBDeviceManager(MATCH_ALL)._list_usb_devices_on_system()
        }
        self.assertEqual(serials, {"900053B3E984FA19", "0000187DA57212DB"})

    def test_partition_numbers(self, *_):
        devs = InMemoryUSBDeviceManager(MATCH_ALL)._list_usb_devices_on_system()
        parts_by_serial: dict[str, set[int]] = {}
        for d in devs:
            parts_by_serial.setdefault(d.serial, set()).add(d.partition)
        self.assertEqual(parts_by_serial["900053B3E984FA19"], {1, 2})
        self.assertEqual(parts_by_serial["0000187DA57212DB"], {1})

    def test_filter_by_vendor(self, *_):
        mgr = InMemoryUSBDeviceManager(DeviceFilter(manufacterer="SanDisk"))
        devs = mgr._list_usb_devices_on_system()
        self.assertEqual(len(devs), 1)
        self.assertEqual(devs[0].serial, "0000187DA57212DB")

    def test_filter_by_serial(self, *_):
        mgr = InMemoryUSBDeviceManager(DeviceFilter(serial="900053.*"))
        devs = mgr._list_usb_devices_on_system()
        self.assertEqual({d.serial for d in devs}, {"900053B3E984FA19"})

    def test_filter_excludes_everything(self, *_):
        mgr = InMemoryUSBDeviceManager(DeviceFilter(serial="nope-no-match"))
        self.assertEqual(mgr._list_usb_devices_on_system(), [])

    def test_missing_devdir_returns_empty(self, *_):
        shutil.rmtree(TEST_ROOT, ignore_errors=True)
        mgr = InMemoryUSBDeviceManager(MATCH_ALL)
        self.assertEqual(mgr._list_usb_devices_on_system(), [])

    def test_reconcile_adds_then_removes(self, *_):
        mgr = InMemoryUSBDeviceManager(MATCH_ALL)
        mgr.reconcile()
        # 3 partitions, but sda1/sda2 share a serial -> 2 unique known devices
        self.assertEqual(len(mgr._get_known_devices()), 2)

        # unplug the SanDisk stick and reconcile again
        os.remove(os.path.join(BY_ID, "usb-SanDisk_U3_Cruzer_Micro_0000187DA57212DB-0:0-part1"))
        mgr.reconcile()
        self.assertEqual(
            {d.serial for d in mgr._get_known_devices()}, {"900053B3E984FA19"}
        )

    def test_only_first_partition_gets_added(self, *_):
        # sda exposes part1 and part2 with the same (unique) serial; since
        # equality is serial-based, only the first one seen is registered.
        mgr = InMemoryUSBDeviceManager(MATCH_ALL)
        mgr.reconcile()

        sda = [d for d in mgr._get_known_devices() if d.serial == "900053B3E984FA19"]
        self.assertEqual(len(sda), 1)
        self.assertEqual(sda[0].partition, 1)  # part1, discovered before part2


if __name__ == "__main__":
    unittest.main(verbosity=2)
