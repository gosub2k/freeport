docker buildx build --tag freeport-csi-driver .
docker tag freeport-csi-driver:latest localhost:32000/freeport-csi-driver:latest
docker push localhost:32000/freeport-csi-driver:latest
kubectl apply -f deploy/
for pod in $( kubectl get pods -n freeport --no-headers | grep csi | cut -d\  -f 1); do
  kubectl delete -n freeport pod $pod &
done
# for d in $(kubectl get bd --no-headers|cut -d \  -f 1); do kubectl delete bd $d; done
echo
