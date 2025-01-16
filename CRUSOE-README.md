# Crusoe provider fork of Kubernetes Autoscaler

This branch contains a bit of additional tooling to build and release a fork
of the cluster-autoscaler with Crusoe cloud support.

The branch structure is as follows:

- Upstream tracks minor versions as branches `cluster-autoscaler-release-1.xx` and
  releases specific tags like `cluster-autoscaler-release-1.xx.y` (e.g. `1.30.3`)
- The Crusoe branches are based on an upstream tag, like
  `crusoe-cluster-autoscaler-release-1.xx.y` (e.g. `1.30.3`) and releases
  will be named like `crusoe-cluster-autoscaler-release-1.xx.y-crusoe.z`.

See also [CrusoeCloud autoscaler provider](./cluster-autoscaler/cloudprovider/crusoecloud/README.md)