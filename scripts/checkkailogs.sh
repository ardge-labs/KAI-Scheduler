#!/bin/sh
#filter=$1
kubectl logs -f -n kai-scheduler -l app=scheduler | grep "\[GPU"
#$filter

