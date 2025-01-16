#!/usr/bin/env bash

# Copyright 2025 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -e

UPSTREAM_VERSION=$1
TAG_PREFIX=${2:-crusoe-cluster-autoscaler-}
BASE_VERSION="${TAG_PREFIX}${UPSTREAM_VERSION}"

# find the latest tag
NEW_VERSION="${BASE_VERSION}-crusoe.0"
git fetch -q --tags --prune --prune-tags
tags=$(git tag -l ${BASE_VERSION}-crusoe.* --sort=-version:refname)
if [[ ! -z "$tags" ]]; then
  arr=(${tags})
  for val in ${arr[@]}; do
    if [[ "$val" =~ ^${BASE_VERSION}-crusoe\.[0-9]+$ ]]; then
      prev_build=$(echo ${val} | cut -d. -f4)
      new_build=$((prev_build+1))
      NEW_VERSION="${BASE_VERSION}-crusoe.${new_build}"
      break
    fi
  done
fi

echo "Version for this commit: ${NEW_VERSION}"
echo "RELEASE_VERSION=${NEW_VERSION}" >> variables.env

DOCKER_TAG="${UPSTREAM_VERSION}-crusoe.${new_build}"
echo "Docker tag for this commit: ${DOCKER_TAG}"
echo "TAG=${DOCKER_TAG}" >> variables.env
