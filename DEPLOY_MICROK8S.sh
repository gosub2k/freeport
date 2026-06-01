docker buildx build --tag freeport-manager .
docker tag freeport-manager:latest localhost:32000/freeport-provisioner:latest
docker push localhost:32000/freeport-provisioner:latest

kubectl apply -f deploy/
