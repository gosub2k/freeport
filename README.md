# freeport

A [Kubernetes storage class]() provisioner to provision volumes on removable USB drives. Its implemented with a CSI driver and manager (shim) to make the driver work better with Kubernetes (especiall version...).

## How does this all work?

[CSI]() works something like this. In the diagram, things (Kubernetes objects, RPCs, containers) provided by Kubernetes or related projects are in BLUE. Pieces that this code implements are in WHITE. And things that the user should add to make use of the driver are in GREEN (namely, you would need to add a storage class to match the types of USB drives you would add to your system, and a a Pod with a [PVC]() using that storage class to actually use the storage). (Not shown yet: the manager runs as a Daemonset and also makes modifications to the [CSINode]() object and PVs created).

![Architecture diagram](csi_diagram.png)

## Why would you do this? (Design goals)

- Automatically add storage for Pods that lives on a separate partition from the rest of the node. Ie, this is one way eliminate the possibility of Pods volume storage using up all the disk space on the node.
- Low dependency generic way to add storage. For example nfs as a storage provisioner depends on the network and the NFS server, while most Linux include drivers to recognize USB block devices.
- The CSI component allows the K8s scheduler to schedule the pods based on disk space availability and topology info. From the user's point of view it is easier than manually specifying a host path for each pod.
- Migration of stateful set pods - if a node goes down, any pods in a StatefulSet can't run if they are depending on that node for storage (if you are not using a network based storage class, for example, where the Pod should be rescheduled to a node where it can run). Here, if a USB device is unplugged from one node and plugged into another, pods will follow the device.

## Why would you not do this? (Design non goals)

- Storage latency/bandwidth. Not really a consideration, different USB devices have different characteristics, none is the fastest possible storage you can get (limited by the USB bus bandwidth).
- High data durability. It actually conflicts with the Pod migration goal - its nice to be able to have pods be scheduled on the node where the USB device is, but theres no redundancy or availability of data baked in if your USB device flakes out.

## Alternatives

- [TopoLVM](https://github.com/topolvm/topolvm) is almost certainly a more robust solution and could likely be modified to meet the design goals here.
- Restarting the pod in the driver daemonset to force re-registration instead of using the manager modify K8s objects would likely solve some of the issues with `NodeGetInfo` not being polled (below).

## Learnings

### Design

Small details of CSI were surprising and made the design more complex than intended:

  - `NodeGetInfo` is only called once, so it's not friendly to dynamically changing topology — there's no built-in mechanism to tell the kubelet to poll it again. The `NodeGetInfoResponse` is used to populate the  `CSINode` `driver.TopologyKeys` field so if a new type of USB storage is added or removed, this field wont' be up to date:
   - There is this [beta feature](https://kubernetes.io/blog/2025/09/11/kubernetes-v1-34-mutable-csi-node-allocatable-count/) but it doesn't allow mutating Topology keys.
  - Similarly, node labels are populated by the Kubelet after calling `NodeGetInfo` and thats only called once so it doesn't support changing node labels (to tell the scheduler what nodes have what storage available) dynamically.
 - Kubernetes doesn't support changing `PersistentVolume` `nodeAffinity` or `CSINode` `driver.TopologyKeys` in place; both require delete-and-recreate. 
    - There is [mutable node affinity](https://kubernetes.io/blog/2026/01/08/kubernetes-v1-35-mutable-pv-nodeaffinity/) for persistent volumes but its in Alpha and requires explicitly enabling it.

  - The semantics of Topology selectors are not friendly to wildcards - storage classes need to have the model, manufacturer, etc of all available USB drives defined explicitly.

### Other bugs

These issues could maybe been anticipated:

- The mounting code tried to mount a swap partition that happened to be on a USB device.
- Migrating devices by unplugging USB sticks led to dirty filesystems that couldn't be mounted again without `fsck`.

### `csi_driver/cmd/driver` — Node plugin

Runs as a DaemonSet (`freeport-csi-driver`), one per node, alongside a `node-driver-registrar` sidecar. Implements the CSI `Identity` and `Node` services over a Unix socket.

- `NodePublishVolume`: discovers mounted USB devices, filters them by the topology segment in the volume's `VolumeContext`, picks a match, creates `<mountpoint>/<volumeID>` on the device, and bind-mounts it into the pod.
- `NodeUnpublishVolume`: unmounts and removes the target path (tolerates "already gone").
- `NodeGetInfo` intentionally reports no `AccessibleTopology` — that's the manager's job (see below).
- Runs `privileged: true`, `hostPID: true`, with the host `/` bind-mounted as `HOST_ROOT` so it can see the real `/dev`, `/sys`, and `/proc/1/mounts`.

### `csi_driver/cmd/controller` — Controller plugin

Runs as a single Deployment (`freeport-csi-controller`) alongside an `external-provisioner` sidecar. Implements CSI `Identity` and `Controller` services.

- `CreateVolume` doesn't provision any storage itself — it mints a `vol-freeport-<name>` ID and echoes the scheduler-selected topology segment back into `VolumeContext`, which is what tells the node plugin which device class to look for later. Idempotent by name/capacity.
- `ControllerPublishVolume`/`UnpublishVolume` are no-ops (`attachRequired: false`).

### `csi_driver/cmd/manager` — Topology/migration manager

Runs as a DaemonSet (`freeport-manager`), one per node. It's a plain reconcile loop talking to the Kubernetes API and the host filesystem. Responsibilities:

1. **Discover and mount** USB devices, retrying and clearing stale mountpoints.
2. **Label the Node** with `freeport.local/<manufacturer>-<model>=true` per mounted device class.
3. **Sync CSINode topology keys**: `CSINodeDriver.TopologyKeys` is immutable in place, the manager removes and re-adds the driver's `CSINodeDriver` entry (with retry-on-conflict) whenever the device set changes.
4. **Migrate volumes**: when a device reappears on a different node than its PV's (immutable) `nodeAffinity` says, and a pod using that PV's claim is scheduled elsewhere, the manager strips PV finalizers, deletes and recreates the PV pinned to the current node, then deletes the misplaced pod so its controller reschedules it.
5. **Repair read-only mounts**: if a device mounts read-only (typical after an unclean unplug), forces `fsck -f -y` and remounts.

**How to test:**
- Unit tests: `./TEST.sh` runs `go test ./pkg/manager/...`.
- End-to-end: chainsaw's `topology-followed-broad`/`topology-followed-narrow` tests verify the manager's topology labels actually drive scheduling; migration itself needs manual testing with real hardware (unplug/replug a device between two nodes and observe the PV/pod move) — see `DEBUG.sh` and `test/manual/cleanup-pod.sh` for tooling.

## Running the whole stack 

### Unit tests

```sh

```

### locally (Tilt)

`Tiltfile.tilt` builds the three images (`freeport-csi-controller`, `freeport-csi-driver`, `freeport-manager`) and deploys a kustomize overlay (`KUSTOMIZE_DIR`, defaults to `kustomize/base`). Three launcher scripts target different build setups.

Example usage:

```sh

```

### Through kustomize/kubectl

```sh

```

### End-to-end tests (chainsaw)

`test/chainsaw/*` requires a real cluster with real USB hardware attached — there's no mocked driver for these:

Copy `test/chainsaw/values.yaml.example` to `values.yaml` and fill in `storageClassName`/`narrowStorageClassName` for your cluster before running — there's no fallback, so a missing file fails loudly. `./TEST.sh` checks for the file and runs `chainsaw test --values values.yaml`.

Tests included:

 `basic-provisioning` — PVC binds, pod reads/writes the volume.
- `topology-followed-broad` — bound PV carries the right `freeport.local/*` nodeAffinity, using a StorageClass every node currently qualifies for.
- `topology-followed-narrow` — same, but with a StorageClass only one node satisfies.
- `ephemeral-inline-volume` — exercises the CSI ephemeral (inline `csi:` volume) mode, bypassing PVC/PV/scheduling entirely.
