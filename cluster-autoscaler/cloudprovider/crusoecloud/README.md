# Cluster Autoscaler for Crusoe Cloud

The Crusoe Cloud Provider implementation scales nodes on different pools
attached to a Crusoe Managed Kubernetes (CMK) cluster.

## Deployment

### As an add-on

When creating the CMK cluster, specify the `cluster-autoscaler` add-on.

Nodepool size constraints come from one of two places:

- The nodepool's `autoscaling_config` in the Crusoe Cloud API (`enabled`, `min_node_size`,
  `max_node_size`), set when the nodepool is created or updated.
- A command line argument of the form `--nodes=<min>:<max>:<nodepool>` on the
  `cluster-autoscaler` deployment (created in the `crusoe-system` namespace), where
  `<nodepool>` is the name of the nodepool.

For each nodepool, an enabled `autoscaling_config` overrides `--nodes`; otherwise the
nodepool follows `--nodes`:

1. `autoscaling_config` is enabled: the nodepool is autoscaled within its
   `autoscaling_config` bounds, whether or not it is listed in a `--nodes` argument, and
   any `--nodes` bounds for it are ignored. Bounds of `[0, 0]` are valid and keep the
   nodepool at zero nodes.
2. `autoscaling_config` is disabled or absent: the nodepool is autoscaled within its
   `--nodes` bounds if it is listed in a `--nodes` argument, and is not autoscaled
   otherwise. Any bounds a disabled `autoscaling_config` retains are ignored, so `--nodes`
   remains the way to autoscale a nodepool whose `autoscaling_config` is disabled.

The autoscaler re-reads each nodepool's `autoscaling_config` on every loop, so changes to
it take effect without restarting the autoscaler. A nodepool whose enabled
`autoscaling_config` has invalid bounds (for example, `min_node_size` above
`max_node_size`) is not autoscaled, even if it is listed in a `--nodes` argument, and a
warning is logged.

Per-nodepool `autoscaling_config` is only applicable when a supported cluster autoscaler
version is installed in the cluster. Older versions ignore `autoscaling_config` and manage
only the nodepools listed in a `--nodes` argument.

**Breaking change:** older versions applied default bounds (min=1, max=254) to nodepools
that were not listed in a `--nodes` argument, so the autoscaler could scale nodepools the
user never configured. Nodepools that have neither an enabled `autoscaling_config` nor a
`--nodes` argument are no longer scaled up or down.

### In-cluster

Edit [`examples/cluster-autoscaler.yaml`](./examples/cluster-autoscaler.yaml) to configure
nodepool constraints as above (`autoscaling_config` or the `--nodes` command line argument)
and deploy using kubectl:

```console
kubectl apply -f examples/cluster-autoscaler.yaml
```

## Configuration

Cluster Autoscaler can be configured with 2 options
### Config file
a config file can be passed with the `--cloud-config` flag.  
here is the corresponding JSON schema:
* `project_id`: Crusoe Project Id
* `cluster_id`: CMK Cluster Id
* `access_key`: API Access Key used to manage associated Crusoe resources
* `secret_key`: API Secret Key used to manage associated Crusoe resources
* `api_endpoint`: URL to contact Crusoe cloud, defaults to `https://api.crusoecloud.com/v1alpha5`

### Env variables

The values expected by the autoscaler can also be passed as environment variables:

- `CRUSOE_PROJECT_ID`
- `CRUSOE_CLUSTER_ID`
- `CRUSOE_ACCESS_KEY`
- `CRUSOE_SECRET_KEY`
- `CRUSOE_API_ENDPOINT`

Note that these values are set in the example manifest file from a Kubernetes secret
that should be available in a deployed CMK cluster. An example of the secret is also
provided in the examples/ directory.
