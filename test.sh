#!/usr/bin/env bash
# End-to-end test for the usb-storage provisioner.
#
# Verifies, against a real cluster:
#   1. The DaemonSet labels at least one node with a USB UUID.
#   2. A PVC using that StorageClass binds.
#   3. The provisioned PV has the right annotations, local.path, and nodeAffinity.
#   4. A pod can write to the volume.
#   5. The data survives the writer pod being deleted and a new pod being scheduled.
#   6. /health returns ok.
#
# Usage:
#   ./test.sh                       # defaults: STORAGE_CLASS=usb-local-a, NAMESPACE=dimsum
#   STORAGE_CLASS=usb-local-c ./test.sh
#
# Requires: kubectl pointed at a cluster with the provisioner deployed and at
# least one node with the matching USB UUID labelled by the DaemonSet.

set -euo pipefail

NS="${NAMESPACE:-dimsum}"
PROV_NS="${PROVISIONER_NAMESPACE:-dimsum}"
SC="${STORAGE_CLASS:-usb-local-cruzer}"
TIMEOUT="${TIMEOUT:-120}"

stamp=$(date +%s)
PVC="usb-test-${stamp}"
POD1="usb-test-writer-${stamp}"
POD2="usb-test-reader-${stamp}"

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[34m%s\033[0m\n' "$*"; }
fail()  { red "FAIL: $*"; exit 1; }

cleanup() {
  blue "[cleanup]"
  local pv=""
  pv=$(kubectl -n "$NS" get pvc "$PVC" -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)
  kubectl -n "$NS" delete pod "$POD1" "$POD2" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete pvc "$PVC" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  if [ -n "$pv" ]; then
    kubectl delete pv "$pv" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

blue "[1/7] preflight"
kubectl version --client >/dev/null || fail "kubectl not on PATH"
kubectl cluster-info >/dev/null    || fail "kubectl can't reach a cluster"
kubectl get sc "$SC" >/dev/null    || fail "StorageClass $SC not found"
kubectl -n "$PROV_NS" get ds usb-storage-provisioner >/dev/null \
  || fail "DaemonSet usb-storage-provisioner not in namespace $PROV_NS"

device_class=$(kubectl get sc "$SC" -o jsonpath='{.parameters.deviceClass}')
[ -n "$device_class" ] || fail "StorageClass $SC has no 'deviceClass' parameter"
kubectl get usbdeviceclass "$device_class" >/dev/null \
  || fail "USBDeviceClass $device_class referenced by $SC does not exist"

label="usb-storage.frankencluster.local/class-$device_class"
nodes=$(kubectl get nodes -l "$label=true" -o jsonpath='{.items[*].metadata.name}')
[ -n "$nodes" ] || fail "no nodes labelled $label — is a matching USB plugged into any node?"
green "  ready: SC=$SC, class=$device_class, eligible nodes: $nodes"

blue "[2/7] create PVC + writer pod"
kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $PVC
  namespace: $NS
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: $SC
  resources:
    requests:
      storage: 100Mi
---
apiVersion: v1
kind: Pod
metadata:
  name: $POD1
  namespace: $NS
spec:
  restartPolicy: Never
  containers:
    - name: writer
      image: busybox:1.36
      command: ["sh", "-c", "echo \"marker-$stamp\" > /vol/marker && sleep 3600"]
      volumeMounts:
        - name: data
          mountPath: /vol
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: $PVC
EOF

blue "[3/7] wait for writer Ready"
kubectl -n "$NS" wait pod/"$POD1" --for=condition=Ready --timeout="${TIMEOUT}s" \
  || fail "writer pod did not become Ready in ${TIMEOUT}s"

blue "[4/7] verify PV shape"
pv=$(kubectl -n "$NS" get pvc "$PVC" -o jsonpath='{.spec.volumeName}')
[ -n "$pv" ] || fail "PVC has no bound volume"

provisioner=$(kubectl get pv "$pv" -o jsonpath='{.metadata.annotations.pv\.kubernetes\.io/provisioned-by}')
[ "$provisioner" = "frankencluster.local/usb-storage" ] \
  || fail "wrong provisioner annotation: '$provisioner'"

local_path=$(kubectl get pv "$pv" -o jsonpath='{.spec.local.path}')
[ "$local_path" = "/data/$PVC" ] \
  || fail "wrong local.path: expected /data/$PVC, got '$local_path'"

bound_node=$(kubectl get pv "$pv" \
  -o jsonpath='{.spec.nodeAffinity.required.nodeSelectorTerms[0].matchExpressions[?(@.key=="kubernetes.io/hostname")].values[0]}')
[ -n "$bound_node" ] || fail "PV has no kubernetes.io/hostname nodeAffinity"

case " $nodes " in
  *" $bound_node "*) ;;
  *) fail "PV pinned to node '$bound_node' which is not labelled with $label" ;;
esac
green "  PV=$pv  node=$bound_node  path=$local_path"

blue "[5/7] read marker via writer"
expected=$(kubectl -n "$NS" exec "$POD1" -- cat /vol/marker)
[ "$expected" = "marker-$stamp" ] || fail "marker mismatch in writer: '$expected'"
green "  marker: $expected"

blue "[6/7] delete writer, start reader, verify data persists"
kubectl -n "$NS" delete pod "$POD1" --wait=true >/dev/null

kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $POD2
  namespace: $NS
spec:
  restartPolicy: Never
  containers:
    - name: reader
      image: busybox:1.36
      command: ["sh", "-c", "cat /vol/marker && sleep 3600"]
      volumeMounts:
        - name: data
          mountPath: /vol
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: $PVC
EOF

kubectl -n "$NS" wait pod/"$POD2" --for=condition=Ready --timeout="${TIMEOUT}s" \
  || fail "reader pod did not become Ready in ${TIMEOUT}s"

got=$(kubectl -n "$NS" exec "$POD2" -- cat /vol/marker)
[ "$got" = "$expected" ] || fail "data did not survive: expected '$expected', got '$got'"
green "  data persisted across pod restart"

blue "[7/7] /health endpoint"
ds_pod=$(kubectl -n "$PROV_NS" get pod -l app=usb-storage-provisioner \
  -o jsonpath='{.items[0].metadata.name}')
[ -n "$ds_pod" ] || fail "no provisioner pods found"
health=$(kubectl -n "$PROV_NS" exec "$ds_pod" -- \
  python -c "import urllib.request; print(urllib.request.urlopen('http://localhost:8080/health').read().decode().strip())")
[ "$health" = "ok" ] || fail "/health returned '$health', expected 'ok'"
green "  /health = ok"

green "PASS"
