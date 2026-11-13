# Kubernetes Autoscaler

[![Release Charts](https://github.com/kubernetes/autoscaler/actions/workflows/release.yaml/badge.svg)](https://github.com/kubernetes/autoscaler/actions/workflows/release.yaml) [![Tests](https://github.com/kubernetes/autoscaler/actions/workflows/ci.yaml/badge.svg)](https://github.com/kubernetes/autoscaler/actions/workflows/ci.yaml) [![GoDoc Widget]][GoDoc]

This repository contains autoscaling-related components for Kubernetes.

## What's inside

[Cluster Autoscaler](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler) - a component that automatically adjusts the size of a Kubernetes
Cluster so that all pods have a place to run and there are no unneeded nodes. Supports several public cloud providers. Version 1.0 (GA) was released with kubernetes 1.8.

[Vertical Pod Autoscaler](https://github.com/kubernetes/autoscaler/tree/master/vertical-pod-autoscaler) - a set of components that automatically adjust the
amount of CPU and memory requested by pods running in the Kubernetes Cluster. Current state - beta.

[Addon Resizer](https://github.com/kubernetes/autoscaler/tree/master/addon-resizer) - a simplified version of vertical pod autoscaler that modifies
resource requests of a deployment based on the number of nodes in the Kubernetes Cluster. Current state - beta.

[Charts](https://github.com/kubernetes/autoscaler/tree/master/charts) - Supported Helm charts for components above.

## Contact Info

Interested in autoscaling? Want to talk? Have questions, concerns or great ideas?

Please join us on #sig-autoscaling at https://kubernetes.slack.com/, or join one
of our weekly meetings.  See [the Kubernetes Community Repo](https://github.com/kubernetes/community/blob/master/sig-autoscaling/README.md) for more information.

## Getting the Code

Fork the repository in the cloud:
1. Visit https://github.com/kubernetes/autoscaler
1. Click Fork button (top right) to establish a cloud-based fork.

The code must be checked out as a subdirectory of `k8s.io`, and not `github.com`.

```shell
mkdir -p $GOPATH/src/k8s.io
cd $GOPATH/src/k8s.io
# Replace "$YOUR_GITHUB_USERNAME" below with your github username
git clone https://github.com/$YOUR_GITHUB_USERNAME/autoscaler.git
cd autoscaler
```

Please refer to Kubernetes [Github workflow guide] for more details.

[GoDoc]: https://godoc.org/k8s.io/autoscaler
[GoDoc Widget]: https://godoc.org/k8s.io/autoscaler?status.svg
[Github workflow guide]: https://github.com/kubernetes/community/blob/master/contributors/guide/github-workflow.md

## Crusoe GitLab tooling

The Crusoe GitLab pipeline uses mise and the shared tools components for the
`cluster-autoscaler` module. Other modules and upstream GitHub release/chart
workflows are unchanged. Go is selected from `cluster-autoscaler/go.mod`,
including its `toolchain` directive; the tracked vendor tree remains in use.

```sh
cd cluster-autoscaler
mise trust
mise install
mise run test:provider
mise run go:build
```

From the repository root, module tasks are also available as
`mise run //cluster-autoscaler:test:provider`. The existing upstream Make targets,
including public Docker-builder and release workflows, keep their supported
variables and remain available. Use the existing `make ci` for the upstream
short race suite and its build/clean prerequisites.

GitLab uses the shared artifact publisher for the Linux amd64 runtime at
`$CI_REGISTRY_IMAGE/cluster-autoscaler-amd64`. Images use the shared commit,
branch-slug, and version tags; the default branch also publishes `latest`.
The project mise configuration disables CGO for both shared build paths while
leaving host race tests available. The original module Dockerfiles remain
available to upstream Make callers. GitLab does not create release tags or publish
the upstream Helm chart; the selected module has no chart.
