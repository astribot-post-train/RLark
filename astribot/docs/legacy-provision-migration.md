# Migrate historical SSH hosts and cluster provisions

## Reuse a historical worker

Retry SSH join on the original host. Gateway requires the pinned SSH fingerprint and the matching WireGuard public key. For a worker installed before the marker stored the cluster and node name, Gateway compares the node name from the original installer journal and a fingerprint of the worker's API server and CA against the selected member's native cluster information. The member API must return a native Node NotFound for that name. The host checks the proof again under its local lock before resetting the worker installation or changing the tunnel.

An active installer, a control plane, a current same-named Node, a different API endpoint or CA, or missing evidence stops automatic recovery. Keep the old record and resolve its ownership before trying again.

## Record historical cluster ownership

Use the administrator endpoint `POST /api/v1/cluster-provisions/{name}/migrate-ownership` before clearing a historical ClusterProvision. Independently establish the original ownership from the installation records, an audit record or a backup. Copy each verified object's current `metadata.uid` and `metadata.resourceVersion`. A matching name, current DNS or current resource list alone cannot prove original ownership.

A request for a stopped provision whose original management Cluster is already absent can look like this:

```json
{
  "provision_uid": "original-provision-uid",
  "evidence": "change record CHG-1234",
  "site_network": {"uid": "reviewed-site-network-uid", "resource_version": "764"},
  "credentials": {
    "station-read": {"uid": "reviewed-secret-uid", "resource_version": "738"}
  }
}
```

If the original Cluster still exists, supply its identity as `cluster`. The operation rejects a live Cluster unless its exact instance is included. Supported credential keys are `station-read`, `station-operations`, `release` and `agent-certificate`. Include only resources you verified. A partially removed SiteNetwork declaration can be migrated when its remaining entries agree with the original provision. For a historical control-plane host, also supply `native_cluster_uid` from the original Kubernetes `kube-system` namespace. Later cleanup uses the recorded SSH fingerprint and verifies the native UID on the host before adopting its cleanup marker.

Migration writes ownership metadata and records the administrator and evidence reference in the audit log. It leaves the installation, Secret data and independent NodeAdmission and NetworkEnrollment objects in place. Use the ordinary clear action when ready. A completed provision with a live Cluster still follows the regular cluster-removal workflow first. If a write conflicts, recheck the remaining objects and retry with their current resource versions. Receipts already written for the same object UID can be reused. Retain any record whose original ownership cannot be established.
