#!/bin/sh
#

#show existed first
#sudo k3s ctr image list |grep kai

for img in $(docker images --format '{{.Repository}}:{{.Tag}}' | grep kai-scheduler); do  
  docker save $img | k3s ctr images import -
done

echo "to upgrade, run upgradechart.sh"

