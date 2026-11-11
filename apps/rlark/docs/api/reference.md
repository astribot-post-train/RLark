# API Reference

This page lists the HTTP routes registered by the Gateway in [`pkg/gateway/router.go`](https://github.com/RLinf/RLark/tree/main/apps/rlark/pkg/gateway/router.go). For runnable requests, see [API Examples](examples.md). The machine-readable subset is available as the [OpenAPI specification](swagger.yaml).

Except for `POST /api/v1/auth/login` and `/metrics`, Gateway routes require the JWT returned by login in the `Authorization: Bearer <token>` header. Tokens expire after 8 hours by default; configure the lifetime with `--jwt-token-ttl`. A missing or invalid token returns `401`; an authenticated `user` calling an admin-only route returns `403`.

Admin-only operations are Node and Domain mutations, all certificate, image registry, and Addon routes, system configuration updates, plus StorageClass provider and mutation routes. System configuration reads and all SSH key operations accept either authenticated role.

Authorization combines role with tenant scope. Workflow, Job, Task, and Pod routes are restricted to the tenants the caller belongs to: an out-of-scope `?tenant=` filter or create body returns `403`, and a named object owned by another tenant returns `404` so that the name is not confirmed. An `admin` caller is unrestricted. Every other resource, including SSH keys, is still only role-based and is not isolated by individual user. See [Tenant scope enforcement](../developer-guide/api-conventions.md#51-tenant-scope-enforcement) for the full model.

Path parameters are written as `{name}` below; Gin uses the equivalent `:name` syntax in the router. Namespaced CRD routes require the `namespace` query parameter.

## CRD resources

| Resource | Scope | Routes |
|----------|-------|--------|
| `nodes` | Namespaced | `GET, POST /api/v1/rlinf.io/v1alpha1/nodes`; `GET, PUT, PATCH, DELETE /api/v1/rlinf.io/v1alpha1/nodes/{name}` |
| `workflows` | Cluster | `GET, POST /api/v1/rlinf.io/v1alpha1/workflows`; `GET, PUT, PATCH, DELETE /api/v1/rlinf.io/v1alpha1/workflows/{name}` |
| `jobs` | Cluster | `GET, POST /api/v1/rlinf.io/v1alpha1/jobs`; `GET /api/v1/rlinf.io/v1alpha1/jobs/tags`; `GET, PUT, PATCH, DELETE /api/v1/rlinf.io/v1alpha1/jobs/{name}`; `GET /api/v1/rlinf.io/v1alpha1/jobs/{name}/logs`; `GET /api/v1/rlinf.io/v1alpha1/jobs/{name}/logs/label-values`; `GET /api/v1/rlinf.io/v1alpha1/jobs/{name}/metrics` |
| `tasks` | Namespaced | `GET, POST /api/v1/rlinf.io/v1alpha1/tasks`; `GET, PUT, PATCH, DELETE /api/v1/rlinf.io/v1alpha1/tasks/{name}`; `GET /api/v1/rlinf.io/v1alpha1/tasks/{name}/exposed-ports` reads allocated NodePorts; all methods on `/api/v1/rlinf.io/v1alpha1/tasks/{name}/tensorboard/{path}` |
| `pods` | Namespaced | `GET /api/v1/rlinf.io/v1alpha1/pods`; `GET, PATCH /api/v1/rlinf.io/v1alpha1/pods/{name}`; `GET /api/v1/rlinf.io/v1alpha1/pods/{name}/events`; `GET /api/v1/rlinf.io/v1alpha1/pods/{name}/terminal` |
| `domains` | Cluster | `GET, POST /api/v1/rlinf.io/v1alpha1/domains`; `GET, PUT, PATCH, DELETE /api/v1/rlinf.io/v1alpha1/domains/{name}` |

When creating a Job, the Gateway stores the submitted `metadata.name` as its display name and returns a generated `jo-<16 hexadecimal characters>` resource ID in `metadata.name`. Use the returned ID for subsequent Job API requests.

The Gateway router does not expose CRD status subresource routes. Status is returned as part of the normal resource representation.

The Job `logs` route is implemented and accepts `from`, `to`, `task`, `pod`, `query`, `cursor`, and `order`. `from` and `to` are RFC 3339 timestamps; `order=asc` requests ascending backend results, while any other value uses descending order. When `from` is supplied and a log backend is configured, a successful response contains `source: "backend"`, `entries`, `hasMore`, and `nextCursor`. Otherwise—including backend errors—it falls back to Pod logs and returns `source: "pod"` plus a `pods` array containing `taskName`, `podName`, `phase`, `node`, and `logs`.

`logs/label-values` accepts `label` (default `pod`), `from`, `to`, `task`, and `pod`, and returns `{ "values": [...] }`; without a configured backend the array is empty. Job `metrics` is registered but currently only returns HTTP `501 Not Implemented`.

## Clusters and certificates

Cluster list/detail responses use `id` as the stable Cluster resource name. `name` is the trimmed `spec.displayName`, falling back to the resource name; `region` prefers trimmed `spec.region` and otherwise retains node-derived metadata. Display metadata supports Unicode. Configure it on the management Cluster resource through the Kubernetes API; use `id`, not `name`, in URLs and delivery/scheduling references.

| Method | Path |
|--------|------|
| `GET` | `/api/v1/clusters` |
| `GET` | `/api/v1/clusters/{cluster_id}` |
| `GET` | `/api/v1/certificates/agent` |
| `GET` | `/api/v1/certificates/agent/{cluster_id}` |
| `POST` | `/api/v1/certificates/agent` |
| `POST` | `/api/v1/certificates/revoke` |

Agent certificate issuance returns credentials only after management Secret persistence. Storage errors fail the request; conflicting or ambiguous Secret ownership is rejected. An interrupted response can still leave a certificate stored, so signing retries may issue a different credential. See [certificate management](../deployment.md#92-issuing-agent-certificates).

Certificate revocation requires an administrator and a compatible Server with durable revocation storage. POST JSON with `cert_type` (`x509` or `ssh`), canonical decimal `serial_number`, `subject_key_id` (lowercase hexadecimal for X.509; SSH key ID for SSH), and optional `reason`. A 200 response with `{"persisted":true}` confirms the Server stored the revocation; repeating the same identity is safe. A 502 can mean the outcome is unknown, including when an older Server returns no durable receipt; retry with the same identity after restoring a compatible Server. Revocation affects new authentications and does not terminate existing sessions. The Gateway does not remove or rotate credentials. This source change requires both Gateway and Server upgrades and is not yet deployed at the station.

## Station identities (preview)

`GET /api/v1/node-networks/{name}?cluster_id=<cluster-id>` returns a node's verified WireGuard interface and CIDR addresses with `state` (`current`, `stale`, or `unreported`) and optional server receipt time. It requires the same user authentication and cluster visibility as node listing. A conflicting namespace mapping returns `409`; missing configuration, member identity verification failures, or an unavailable Station API return `503`. It does not return peers or keys. The feature requires the optional Gateway and Station API configuration described in the Station gateway README.

| Method | Path |
|--------|------|
| `GET` | `/api/v1/station/stations?cluster_id={cluster_id}&limit={limit}&continue={token}` |
| `GET` | `/api/v1/station/stations/{name}?cluster_id={cluster_id}` |
| `POST` | `/api/v1/station/rebinds` |
| `GET` | `/api/v1/station/rebinds/{name}?cluster_id={cluster_id}` |
| `GET` | `/api/v1/station/observations?cluster_id={cluster_id}&limit={limit}&continue={token}` |

The two `GET` routes require an administrator. The list reads Station custom resources from the management Kubernetes API; `cluster_id` is the registered Cluster object name, which the Gateway resolves to its authoritative `spec.agentNamespace`. Do not pass a namespace as `cluster_id`. Pagination accepts `limit` from 1 through 500 (default 200) and the opaque `continue` token returned by the API. Station names are namespaced, so the detail route requires `cluster_id`. A conflicting Cluster-to-namespace mapping returns `409`; an unavailable management API returns `502`. These GET endpoints are read-only.

`GET /api/v1/station/observations` requires `cluster_id` and returns the latest server-verified snapshots in that Cluster's mapped namespace. These records include Node UIDs suitable for selecting a rebind target; the controller still rereads the member Node before applying an operation. `POST /api/v1/station/rebinds` is an administrator-only preview operation endpoint. Submit `cluster_id`, stable `request_id`, `station_name`, the displayed `station_uid` and `expected_binding_version`, `target_node_name`, `target_node_uid`, and an operator `reason`; `target_system_uuid` is optional evidence. The request must be strict JSON, at most 16 KiB, and contain no unknown fields. The Gateway resolves namespace from the registered Cluster and rejects a stale Station UID, retired Station, or changed binding version. It creates a deterministic `StationRebind` operation and returns `202`; retrying the same ID and payload returns the same operation, while reusing the ID with different content returns `409`. `GET /api/v1/station/rebinds/{name}` returns current asynchronous status. The controller independently reads the target Node from the member cluster, validates its UID, and rejects a UID already bound to a different Station before applying the change. An accepted operation is asynchronous; `202` does not mean the rebind has completed. The console submits rebinds for `Conflict` Stations and polls the operation; actual completion requires the Station CRDs and controller to be installed.

Station discovery and Rebind reconciliation share a serialized coordinator under controller-manager leader election, and race tests verify one owner when they concurrently process the same Node UID. Leader failover and live CRD operation are not yet tested, so this preview endpoint is not a production identity guarantee.

## Authentication and SSH keys

| Method | Path |
|--------|------|
| `POST` | `/api/v1/auth/login` |
| `GET` | `/api/v1/ssh-user-keys` |
| `POST` | `/api/v1/ssh-user-keys` |
| `DELETE` | `/api/v1/ssh-user-keys/{index}?user={user}` |

## API reference metadata

| Method | Path |
|--------|------|
| `GET` | `/api/v1/api-reference` |

The authenticated API reference metadata endpoint returns localized section titles, endpoint methods and paths, descriptions, and response examples. The Web UI uses this endpoint as its API reference data source instead of maintaining a separate endpoint list.

## Gateway observability

| Method | Path |
|--------|------|
| `GET` | `/metrics` |

`GET /metrics` exposes Gateway metrics in Prometheus text format.

## Images, image registries, and system configuration

| Method | Path |
|--------|------|
| `GET` | `/api/v1/images` |
| `GET, POST` | `/api/v1/image-registries` |
| `GET, PUT, DELETE` | `/api/v1/image-registries/{id}` |
| `GET` | `/api/v1/image-registries/{id}/password` |
| `GET` | `/api/v1/image-registries/{id}/repositories` |
| `GET` | `/api/v1/image-registries/{id}/tags?repository={name}` |
| `GET` | `/api/v1/image-registries/{id}/manifest?repository={name}&tag={tag}` |
| `GET, PUT` | `/api/v1/system-config` |

System configuration is split into `ssh` and `log` categories. `ssh.jumpHost` must be a hostname or IP address without a scheme, user, path, or port; `ssh.jumpPort`, when set, must be an integer from 1 to 65535. Set `log.backend` to `none` to disable historical log queries, or to `sls` with the required SLS connection fields. Sensitive SLS credentials are returned as `****`; sending that placeholder back preserves the stored value. A successful `PUT` returns the effective masked configuration.

The optional `deployment` category follows the relevant subset of the `rlarkadm` `DeployConfig` structure and controls defaults in the deployment YAML shown after signing a data-plane Agent cluster. It accepts only `controlPlaneAddress`, `sshAddress`, `insecureSkipTlsVerify`, and the Kubernetes Agent fields `kubeconfig`, `agentImage`, `image`, `imagePullPolicy`, `imagePullSecrets`, and `containerdSocket`. Control-plane components, database, certificate, Docker, and Raw deployment fields are rejected. If `controlPlaneAddress` is empty, the signing API's Server address is used. Image pull policy may be `Always`, `IfNotPresent`, or `Never`.

Image registry credentials use an immutable `ir-<16 lowercase hexadecimal characters>` ID; the display `name` may be duplicated or changed. Create requests require `name`, `registry`, `username`, `password`, and `clusterSelection`:

```json
{"name":"Production Harbor","registry":"harbor.example.com","username":"robot","password":"secret","clusterSelection":{"mode":"Selected","clusters":["cluster-a"]}}
```

`clusterSelection.mode` is `None` (store only), `Selected` (one or more logical cluster names), or `All` (current and future clusters). `clusters` must be empty for `None` and `All`. Ordinary credential responses omit the password and include `id`. `POST` returns `201 Created`. On `PUT`, omitting `password` preserves it; an explicitly empty password is invalid. `DELETE` returns `202 Accepted` because Replication and Delivery remove distributed Secrets asynchronously.

The list and detail omit passwords. Administrators may explicitly call `GET /api/v1/image-registries/{id}/password`, returning `{"password":"..."}` with `Cache-Control: no-store`. Authenticated attempts produce a structured audit log with actor, registry ID and status, never the password.

The three read-only browse endpoints require an administrator. Configure the Gateway with `--registry-browse-allowed-hosts=registry.example.com:5443` (comma-separated exact `host[:port]` values or IP CIDRs); the default empty list disables browsing. A CIDR permits IP-literal hosts on any port; hostnames are not resolved for CIDR matching. Restrict CIDRs to trusted networks. Only explicitly allowed HTTPS origins are contacted, redirects and proxies are not followed, and stored credentials remain on the Gateway. The Gateway must be able to reach the registry and trust its TLS certificate using its system CA store; install a private CA in the Gateway environment before starting it. Do not disable certificate verification. Registry catalog, paginated tags and manifest HEAD with a `sha256` digest and Basic authentication are required; token-authentication challenges and registries that omit catalog support are not supported. Each catalog/tag page has at most 50 entries; pass the returned `next` value as `last` for the next page. Registry failures return 502, not an empty result. Existing `GET /api/v1/images` is Job image usage history, not registry inventory. Image references and digests can change between requests; pin verified digests for releases.

## Storage

| Method | Path |
|--------|------|
| `GET, POST` | `/api/v1/storage/storageclass` |
| `PUT, DELETE` | `/api/v1/storage/storageclass/{name}` |
| `GET` | `/api/v1/storage/storageclass/provider` |
| `GET` | `/api/v1/storage/storageclass/{name}/list` |
| `POST` | `/api/v1/storage/storageclass/{name}/upload` |
| `GET, DELETE` | `/api/v1/storage/storageclass/{name}/object/{key}` |

StorageClass routes manage control-plane declarations. Replication and Delivery controllers asynchronously distribute generated rclone CSI resources to the configured target clusters; listing returns managed declarations rather than live Agent queries. File routes access the declared bucket directly.

## Addons

| Method | Path |
|--------|------|
| `GET` | `/api/v1/addons` |
| `GET` | `/api/v1/addons/{name}` |
| `GET` | `/api/v1/installed-addons` |
| `GET, POST` | `/api/v1/clusters/{cluster_id}/addons` |
| `GET, PUT, DELETE` | `/api/v1/clusters/{cluster_id}/addons/{name}` |
