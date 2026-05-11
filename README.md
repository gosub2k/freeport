# usb-storage-provisioner

A small Kubernetes storage provisioner for the frankencluster: USB drives
plugged directly into nodes, no NFS, no networked storage.

## What it does

The controller is a DaemonSet — one pod per node. Each pod:

* Reads every `USBDeviceClass` CR (cluster-scoped). A class defines the
  match criteria — by product (`vendor` + `model`) or by serial allowlist —
  plus optional initialisation rules.
* Detects USB block devices on its node by reading `/host/sys/block` and
  `/host/proc/1/mountinfo` (no external binaries needed). Matches each
  device against the registered classes.
* Records every recognised device in an **etcd-backed registry**: one
  `USBDevice` CR (`frankencluster.local/v1`) per serial, with the
  device's identity in `.spec` and live state (`currentNode`, `lastSeen`,
  `phase`, `fsUUID`, `mountpoint`) on the `.status` subresource. Inspect
  with `kubectl get usbdevice`.
* **Initialises** a blank filesystem with the directory structure the
  model declares, and drops a `.usb-storage-init` marker so it only runs
  once per drive.
* Labels the node with `usb-storage.frankencluster.local/class-<className>=true`
  for every class that has a matching device locally. The scheduler uses
  these labels (via `allowedTopologies` on the StorageClass) to place pods
  on nodes that can satisfy the volume.
* Provisions Kubernetes `local` PVs (path `/data/<pvc-name>`,
  `nodeAffinity` pinned to this node) when a PVC with the matching
  StorageClass + selectedNode arrives.
* **Migrates PVs** when a drive moves to a different node: the PV is
  recreated with new nodeAffinity, and pods using the affected PVCs are
  evicted so their controllers reschedule them onto the new node.
* Emits Kubernetes Events for the major lifecycle moments:
  `Provisioned`, `DeviceInserted`, `DeviceInitialized`, `DeviceRemoved`,
  `DeviceMigrated`, `PVMigrated`.
* Exposes `/health` on port 8080.

## What it is not

Not a full CSI driver. This is a small external provisioner that emits
Kubernetes' built-in `local` PV type. ~600 lines of Python instead of a
CSI gRPC server. If you need block volumes, snapshots, expansion, or
multi-attach, swap this out.

## Layout

```
provisioner.py                  # the provisioner, runs in the DaemonSet
Dockerfile                      # python:3.12-slim + kubernetes + PyYAML
requirements.txt
test.sh                         # end-to-end smoke test (kubectl-based)
deploy/
  crd-usbdevice.yaml            # USBDevice CRD (per-drive runtime registry)
  crd-usbdeviceclass.yaml       # USBDeviceClass CRD (user-facing match policy)
  rbac.yaml                     # Namespace + SA + ClusterRole/Binding + Role/Binding
  daemonset.yaml                # DaemonSet + headless Service
  usbdeviceclass-example.yaml   # two example classes
  storageclass-example.yaml     # StorageClass that references a class
```

## Prerequisites on each node

None. Just plug the USB in. The daemon:

1. detects the device via `/sys/block`,
2. matches it against your `USBDeviceClass` CRs,
3. if matched and unformatted, runs `mkfs.ext4` on it (set `initialize.autoFormat: true` on the class),
4. mounts it at `/data` in the host's mount namespace,
5. creates the class's declared subdirectories,
6. registers it as a `USBDevice` CR in the cluster.

All host operations run via `nsenter --target 1`, so the container needs
`privileged: true` + `hostPID: true` (already set in `deploy/daemonset.yaml`).
The container does **not** need `e2fsprogs` or `util-linux` shipped inside —
nsenter switches into PID 1's namespaces, so it uses the host's `mkfs.ext4`
and `mount` binaries directly.

> Safety: `autoFormat` only fires when no filesystem signature is found on
> the device (i.e. nothing in `/dev/disk/by-uuid` points at it). A drive that
> already holds data, even with a different model, is never reformatted.

## Install

1. Build & push the image:
   ```bash
   docker build -t YOUR_REGISTRY/usb-storage-provisioner:latest .
   docker push YOUR_REGISTRY/usb-storage-provisioner:latest
   ```

2. Edit `deploy/daemonset.yaml` and replace `REPLACE_ME/usb-storage-provisioner:latest`
   with your image.

3. Edit `deploy/usbdeviceclass-example.yaml` (or create your own) so the
   `match:` blocks describe the actual USBs you want the controller to
   accept. `vendor` and `model` come from
   `/sys/block/<dev>/device/{vendor,model}` — easiest way to check them is
   `cat /sys/block/sdX/device/{vendor,model}` on a node.

4. Apply everything:
   ```bash
   kubectl apply -f deploy/
   ```

5. Verify:
   ```bash
   kubectl -n dimsum get ds usb-storage-provisioner
   kubectl -n dimsum logs -l app=usb-storage-provisioner --tail=40
   kubectl get usbdeviceclass            # the classes you configured
   kubectl get usbdevice                 # one CR per physical drive seen
   kubectl get usbdevice -o wide         # adds Product / FS-UUID / Serial
   kubectl get nodes --show-labels | grep usb-storage
   kubectl -n dimsum get events
   ```

## Device classes

The controller's behaviour is driven by `USBDeviceClass` CRs. Match by
product or by serial allowlist; both can be set, all set fields must
pass. UUIDs never appear in user config.

```yaml
# match by product — any drive of this make/model qualifies
apiVersion: frankencluster.local/v1
kind: USBDeviceClass
metadata:
  name: cruzer-blade
spec:
  match:
    vendor: "SanDisk"
    model: "Cruzer Blade"
  initialize:
    autoFormat: true
    fsLabel: "frankenstore"
    directories: [projects, kv, files]
---
# match by serial — explicit allowlist
apiVersion: frankencluster.local/v1
kind: USBDeviceClass
metadata:
  name: backup-set
spec:
  match:
    serials:
      - "AA00112233445566"
      - "BB00778899AABBCC"
  initialize:
    directories: [backups]
```

Behaviour:
* On first match for an unseen serial, the controller creates a
  `USBDevice` CR for the drive and emits a `DeviceInserted` event.
* If the matched drive is unformatted and the class has
  `initialize.autoFormat: true`, the controller runs `mkfs.ext4` on it.
* Once mounted at `/data` and blank (no init marker, no entries other
  than `lost+found`), the controller creates each listed subdirectory
  and writes `.usb-storage-init` — this fires `DeviceInitialized`.

Add or change classes with `kubectl apply -f`. The daemons re-read the
class list at the start of each reconcile.

## StorageClass mapping

A `StorageClass` references a class by name. One StorageClass can yield
PVs from any drive matching the class, on any node where one is plugged in:

```yaml
parameters:
  deviceClass: "cruzer-blade"
allowedTopologies:
  - matchLabelExpressions:
      - key: usb-storage.frankencluster.local/class-cruzer-blade
        values: ["true"]
```

A class can be referenced by multiple StorageClasses (e.g. one with
`reclaimPolicy: Retain` and one with `Delete`), and a class can match
many physical drives — the relationship is one-class-to-many-drives.

## Adding a new USB drive

1. **Make sure a class matches it.** If the drive is the same make/model as
   one you've already accepted, you're done — there's nothing to add. If
   it's new, either:
   * pick any node, check `cat /sys/block/sdX/device/{vendor,model}` and
     add a class with that `vendor` + `model`, or
   * add the drive's serial to an existing class's `match.serials` list.
   Then `kubectl apply -f` the class.
2. **Plug it in.** Within `POLL_INTERVAL` (10s) the daemon on that node
   should log `FORMAT /dev/sdX` (if unformatted) and `MOUNT /dev/sdX at
   /data`, then emit `DeviceInserted` + `DeviceInitialized`:
   ```bash
   kubectl -n dimsum get events --sort-by=.lastTimestamp | tail -10
   kubectl get usbdevice
   ```
3. **Use it.** Any PVC with `storageClassName` pointing at a StorageClass
   whose `parameters.deviceClass` matches the class will now be eligible
   for provisioning on this node.

## Device migration

If a USB is unplugged from one node and re-inserted into another:

1. The daemon on the old node notices the device is gone, marks the
   registry entry `status: removed`, emits `DeviceRemoved` — but
   *keeps* `currentNode` so we know where it was.
2. The daemon on the new node sees the same serial with `currentNode`
   pointing at the old node. It logs at WARNING level with a
   `MIGRATION` prefix, emits `DeviceMigrated`, and for every PV
   labelled with that serial:
     * Deletes any pod using the PV's PVC (so the mount is released
       and the pod's controller will reschedule it).
     * Strips the `kubernetes.io/pv-protection` finalizer.
     * Deletes the PV and recreates it with the same name, same
       `claimRef`, but new `nodeAffinity`.
     * Emits `PVMigrated` once the new PV is in.

Things to know about migration:

* Bare pods (no controller) are deleted and gone. Anything you actually
  care about should be a StatefulSet or Deployment.
* The `local` PV `nodeAffinity` is immutable, hence the delete-and-recreate.
* Migration relies on the old node's registry entry; it does not require
  the old node to be online.
* Look for `MIGRATION` in the logs:
  ```bash
  kubectl -n dimsum logs -l app=usb-storage-provisioner --all-containers \
    | grep MIGRATION
  ```

## Health endpoint

`GET /health` on port 8080 returns:
* `200 ok` if the reconcile loop has completed within the last 60s.
* `503 stale` otherwise.

## Test

`./test.sh` runs an end-to-end smoke test against the current cluster: PVC
binds, PV has correct shape, data survives pod restart, `/health` returns
ok. It does **not** exercise migration — that requires two nodes and a
physical USB move, which there's no good way to script.

```bash
./test.sh
STORAGE_CLASS=usb-local-c ./test.sh
```

## Operations

* **Reclaim policy.** Example StorageClass uses `Retain`; switch to `Delete`
  to let the daemon `rm -rf` the data dir on PV release.
* **Multiple USBs per node.** Supported in principle, but all drives have
  to be mounted under `/data` — one drive per node is the practical limit.
* **Hot-unplug while a pod is using a PV.** The pod will fail I/O.
  Migration only kicks in once the drive shows up on another node.
* **No replication.** Local storage. If a USB dies the data is gone.

## Troubleshooting

* `Pending` PVC, no `selected-node` annotation — no node has the matching
  `usb-storage.frankencluster.local/class-<className>=true` label. Check
  `kubectl get nodes --show-labels` and that a matching device appears in
  `kubectl get usbdevice`.
* `selected-node` set but no PV — check the daemon logs on that node.
* `Device inserted` event missing but drive is plugged in — the device
  probably doesn't match any class. Check
  `cat /sys/block/sdX/device/{vendor,model}` and adjust your
  USBDeviceClass.
* `Migration recreated PV …` followed by a stuck pod — the PVC re-bound
  but the pod's controller hasn't rescheduled. Check it has a controller
  (StatefulSet / Deployment), not a bare Pod.
