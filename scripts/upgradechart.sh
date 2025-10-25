#!/bin/sh
helm uninstall kai-scheduler -n kai-scheduler
kubectl -n kai-scheduler delete jobs crd-upgrader
helm upgrade -i kai-scheduler -n kai-scheduler  --create-namespace --set "global.gpuSharing=true" --set "scheduler.additionalArgs[0]=--v=4" ./charts/kai-scheduler-0.0.0.tgz

