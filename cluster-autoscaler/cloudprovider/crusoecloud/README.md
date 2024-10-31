# Cluster Autoscaler for Crusoe Cloud

The Crusoe Cloud Provider implementation scales nodes on different pools
attached to a Crusoe Managed Kubernetes (CMK) cluster.

## Configuration

Cluster Autoscaler can be configured with 2 options
### Config file
a config file can be passed with the `--cloud-config` flag.  
here is the corresponding JSON schema:
* `cluster_id`: CMK Cluster Id
* `api_key`: API Key used to manage associated Crusoe resources
* `region`: Region where the control-plane is runnning
* `api_url`: URL to contact Crusoe cloud, defaults to `api.crusoecloud.com`

### Env variables

The values expected by the autoscaler are the same as above

- `CLUSTER_ID`
- `CRUSOE_API_KEY`
- `CRUSOE_REGION`
- `CRUSOE_API_URL`

## Notes

TODO: k8s nodes are identified through `node.Spec.ProviderId`, the Crusoe node name or id MUST NOT be used.
