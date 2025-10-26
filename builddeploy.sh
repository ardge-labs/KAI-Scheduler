#!/bin/sh
make build

# Import all locally built images to k3s containerd
echo "Importing images to k3s..."
docker images --format "{{.Repository}}:{{.Tag}}" | grep "registry/local/kai-scheduler.*:0.0.0" | while read image; do
    echo "  Importing $image"
    docker save "$image" | sudo k3s ctr images import -
done

helm package ./deployments/kai-scheduler -d ./charts
helm uninstall kai-scheduler -n kai-scheduler
kubectl -n kai-scheduler delete jobs crd-upgrader
helm upgrade -i kai-scheduler -n kai-scheduler  --create-namespace --set "global.gpuSharing=true" --set "scheduler.additionalArgs[0]=--v=4" --set "crdupgrader.image.pullPolicy=Never" ./charts/kai-scheduler-0.0.0.tgz

