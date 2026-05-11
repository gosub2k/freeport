# usb-storage-provisioner

A small Kubernetes storage provisioner for the frankencluster: USB drives
plugged directly into nodes, no NFS, no networked storage.

## What it does

The controller is a DaemonSet — one pod per node. Each pod:

* Loads a list of **device models** from a ConfigMap. A model is a set of
  fields the kernel exposes when a USB is inserted (vendor, model, serial,
  filesystem UUID) plus optional initialisation rules.
* Detects USB block devices on its node by reading `/host/sys/block` and
  `/host/proc/1/mountinfo` (no external binaries needed). Matches each
  device against the registered models.
* Records every recognised device in an **etcd-backed registry**: one
  `USBDevice` CR (`frankencluster.local/v1`) per serial, with the
  device's identity in `.spec` and live state (`currentNode`, `lastSeen`,
  `phase`, `fsUUID`, `mountpoint`) on the `.status` subresource. Inspect
  with `kubectl get usbdevice`.
* **Initialises** a blank filesystem with the directory structure the
  model declares, and drops a `.usb-storage-init` marker so it only runs
  once per drive.
* Labels the node with `usb-storage.frankencluster.local/uuid-<UUID>=true`
  so the scheduler can place pods that need this drive.
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
  crd-usbdevice.yaml            # USBDevice CRD (registry)
  rbac.yaml                     # Namespace + SA + ClusterRole/Binding + Role/Binding
  configmap-models.yaml         # the device-model list
  daemonset.yaml                # DaemonSet + headless Service
  storageclass-example.yaml     # one StorageClass per drive
```

## Prerequisites on each node

None. Just plug the USB in. The daemon:

1. detects the device via `/sys/block`,
2. matches it against your `usb-storage-models` ConfigMap,
3. if matched and unformatted, runs `mkfs.ext4` on it (set `initialize.autoFormat: true` in the model),
4. mounts it at `/data` in the host's mount namespace,
5. creates the model's declared subdirectories,
6. registers it in the etcd registry.

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

3. Edit `deploy/configmap-models.yaml` so the `match:` blocks describe the
   actual USBs you want the controller to recognise. `vendor` and `model`
   come from `/sys/block/<dev>/device/{vendor,model}` — easiest way to
   check them is `cat /sys/block/sdX/device/{vendor,model}` on a node.

4. Apply everything:
   ```bash
   kubectl apply -f deploy/
   ```

5. Verify:
   ```bash
   kubectl -n dimsum get ds usb-storage-provisioner
   kubectl -n dimsum logs -l app=usb-storage-provisioner --tail=40
   kubectl get nodes --show-labels | grep usb-storage
   kubectl get usbdevice                 # registry — one CR per known drive
   kubectl get usbdevice -o wide         # adds Serial + FS-UUID columns
   kubectl -n dimsum get events
   ```

## Device models

The controller's behaviour is driven by the `usb-storage-models` ConfigMap.
Sample entry:

```yaml
models:
  - name: frankencluster-usb-v1
    match:
      vendor: "SanDisk"
      model: "Cruzer Blade"
    initialize:
      directories:
        - projects
        - kv
        - files
```

Behaviour:
* A device matches a model when every `match` field equals the
  corresponding kernel-reported field exactly.
* On first match for an unseen serial, the controller registers the
  device in `usb-storage-registry` and emits a `DeviceInserted` event.
* If the device is mounted at `/data` and the filesystem is blank
  (no init marker, no entries except `lost+found`), the controller
  creates each listed directory and writes `.usb-storage-init` — this
  fires `DeviceInitialized`.

To add a new model just edit the ConfigMap and `kubectl apply -f` it.
The daemons re-read the file at the start of each reconcile.

## Adding a new USB drive

1. **Make sure a model matches it.** Pick any node, check
   `cat /sys/block/sdX/device/{vendor,model}`, confirm those values appear
   in `usb-storage-models`. If they don't, add a model entry and
   `kubectl apply -f deploy/configmap-models.yaml`.
2. **Plug it in.** Within `POLL_INTERVAL` (10s) you should see in the daemon
   log on that node: `FORMAT /dev/sdX`, `MOUNT /dev/sdX at /data`, and a
   `DeviceInserted` + `DeviceInitialized` event:
   ```bash
   kubectl -n dimsum get events --sort-by=.lastTimestamp | tail -10
   ```
3. **Find the filesystem UUID** the daemon assigned:
   ```bash
   kubectl get usbdevice -o wide
   # or, by serial:
   kubectl get usbdevice <sanitised-serial> -o jsonpath='{.status.fsUUID}'
   ```
4. **Create a StorageClass** for the drive. Copy
   `deploy/storageclass-example.yaml`, rename it (e.g. `usb-local-c.yaml`),
   replace both `REPLACE-WITH-FILESYSTEM-UUID` occurrences, apply.
5. **Use it** from a PVC in the `dimsum` namespace with
   `storageClassName: usb-local-c`.

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
  `usb-storage.frankencluster.local/uuid-<UUID>=true` label. Check
  `kubectl get nodes --show-labels` and that the device is in
  `/dev/disk/by-uuid` on the node.
* `selected-node` set but no PV — check the daemon logs on that node.
* `Device inserted` event missing but drive is plugged in — the device
  probably doesn't match any model. Check
  `cat /sys/block/sdX/device/{vendor,model}` and adjust the ConfigMap.
* `Migration recreated PV …` followed by a stuck pod — the PVC re-bound
  but the pod's controller hasn't rescheduled. Check it has a controller
  (StatefulSet / Deployment), not a bare Pod.
