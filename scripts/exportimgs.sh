#!/bin/sh
#

#show existed first
#sudo k3s ctr image list |grep kai

for img in $(docker images --format '{{.Repository}}:{{.Tag}}' | grep kai-scheduler); do  
  bnm=$(basename "$img" | cut -d: -f1)
  docker save $img -o imgs/${bnm}.tar 
 # | k3s ctr images import -
done

