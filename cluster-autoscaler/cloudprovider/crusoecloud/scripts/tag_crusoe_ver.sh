#!/usr/bin/env bash
set -e

UPSTREAM_VERSION=$1
TAG_PREFIX=${2:-crusoe-cluster-autoscaler-}
BASE_VERSION="${TAG_PREFIX}${UPSTREAM_VERSION}.${MINOR_VERSION}"

# find the latest tag
NEW_VERSION="${BASE_VERSION}-crusoe.0"
git fetch -q --tags --prune --prune-tags
tags=$(git tag -l ${BASE_VERSION}-crusoe.* --sort=-version:refname)
if [[ ! -z "$tags" ]]; then
  arr=(${tags})
  for val in ${arr[@]}; do
    if [[ "$val" =~ ^${BASE_VERSION}-crusoe\.[0-9]+$ ]]; then
      prev_build=$(echo ${val} | cut -d. -f3)
      new_build=$((prev_build+1))
      NEW_VERSION="${BASE_VERSION}-crusoe.${new_build}"
      break
    fi
  done
fi

echo "Version for this commit: ${NEW_VERSION}"
echo "RELEASE_VERSION=${NEW_VERSION}" >> variables.env
