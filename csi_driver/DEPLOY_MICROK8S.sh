#!/bin/bash
# 
echome() {
  echo -------------- $1 ----------
  
}

echome "building the driver"
docker buildx build -f Dockerfile.node --tag freeport-csi-driver .
docker tag freeport-csi-driver:latest localhost:32000/freeport-csi-driver:latest
docker push localhost:32000/freeport-csi-driver:latest
echome "building the controller"
docker buildx build -f Dockerfile.controller --tag freeport-csi-controller .
docker tag freeport-csi-controller:latest localhost:32000/freeport-csi-controller:latest
docker push localhost:32000/freeport-csi-controller:latest
echome "redeploy"
kubectl delete -f deploy/
kubectl apply -f deploy/
echome "deleting stuff inside pods so that they will not hang on deletion if driver broken"
# hack
sudo find /var/snap/microk8s/common/var/lib/kubelet/pods -type f -name hello.txt -exec rm -v {} \;
if [[ ! -z "$OTHER_HOST" ]]; then
  ssh $OTHER_HOST "iudo find /var/snap/microk8s/common/var/lib/kubelet/pods -type f -name hello.txt -exec rm -v {} \;"
fi
echome "cleanup prev pods"
for pod in $( kubectl get pods -n freeport --no-headers | grep test-csi | cut -d\  -f 1); do
  #kubectl exec "$pod" -- sh -c '[ -d /data ] && rm -fv /data/hello.txt'
  kubectl delete -n freeport pod $pod 
done
echome "redploy test"
# for d in $(kubectl get bd --no-headers|cut -d \  -f 1); do kubectl delete bd $d; done
kubectl delete -f ../test/inline.yaml
kubectl apply -f ../test/inline.yaml
