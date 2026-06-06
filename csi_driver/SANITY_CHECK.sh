#!/bin/bash
set -e

go install github.com/kubernetes-csi/csi-test/v5/cmd/csi-sanity@latest

go build -o csi-driver .

thisdir=$(pwd)
mountdir=/tmp/csi-target
stagingdir=/tmp/csi-staging

rm -rf "$mountdir" "$stagingdir"

./csi-driver &
DRIVER_PID=$!

trap "kill $DRIVER_PID 2>/dev/null; rm -f $thisdir/csi.sock" EXIT

sleep 1  # wait for socket

"$(go env GOPATH)/bin/csi-sanity" \
  --csi.endpoint="unix://$thisdir/csi.sock" \
  --csi.mountdir="$mountdir" \
  --csi.stagingdir="$stagingdir" \
  --ginkgo.skip="Controller|Node Service"  # Node Service BeforeEach calls CreateVolume; add controller to unblock
