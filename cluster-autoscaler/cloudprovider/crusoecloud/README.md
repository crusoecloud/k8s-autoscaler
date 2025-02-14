# Cluster Autoscaler for Crusoe Cloud

The Crusoe Cloud Provider implementation scales nodes on different pools
attached to a Crusoe Managed Kubernetes (CMK) cluster.

## Deployment

### As an add-on

When creating the CMK cluster, specify the `cluster-autoscaler` add-on.

In order to configure nodepool size constraints, edit the `cluster-autoscaler` deployment
(which will be created in the `crusoe-system` namespace) and add one or more command line
arguments of the form `--nodes=<min>:<max>:<nodepool>` where `<nodepool>` is the name of
the nodepool.

### In-cluster

Edit [`examples/cluster-autoscaler.yaml`](./examples/cluster-autoscaler.yaml) to configure
nodepool constraints as above (the `--nodes` command line argument) and deploy using kubectl:

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
