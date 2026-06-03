docker buildx build --tag freeport-manager .
docker tag freeport-manager:latest localhost:32000/freeport-provisioner:latest
docker push localhost:32000/freeport-provisioner:latest
for pod in $( kubectl get pods -n freeport --no-headers | cut -d\  -f 1); do
  kubectl delete -n freeport pod $pod &
done
kubectl apply -f deploy/
for d in $(kubectl get bd --no-headers|cut -d \  -f 1); do kubectl delete bd $d; done
