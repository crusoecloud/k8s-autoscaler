# Cluster Autoscaler for Crusoe Cloud

The Crusoe Cloud Provider implementation scales nodes on different pools
attached to a Crusoe Managed Kubernetes (CMK) cluster.

## Configuration

Cluster Autoscaler can be configured with 2 options
### Config file
a config file can be passed with the `--cloud-config` flag.  
here is the corresponding JSON schema:
* `api_url`: URL to contact Crusoe cloud, defaults to `https://api.crusoecloud.com/v1alpha5`
* `access_key`: API Access Key used to manage associated Crusoe resources
* `secret_key`: API Secret Key used to manage associated Crusoe resources
* `region`: Region where the control-plane is runnning
* `project_id`: Crusoe Project Id
* `cluster_id`: CMK Cluster Id

### Env variables

The values expected by the autoscaler are the same as above

- `CRUSOE_API_URL`
- `CRUSOE_ACCESS_KEY`
- `CRUSOE_SECRET_KEY`
- `CRUSOE_REGION`
- `CRUSOE_PROJECT_ID`
- `CLUSTER_ID`

## Notes

TODO: k8s nodes are identified through `node.Spec.ProviderId`, the Crusoe node name or id MUST NOT be used.
