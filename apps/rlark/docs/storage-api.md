# Storage API Documentation

## Overview

The Storage API manages RLark StorageClass declarations across multiple clusters. The Gateway stores each declaration in the management plane, and the Replication and Delivery controllers asynchronously distribute the generated rclone CSI StorageClass and its Secret to the selected data-plane clusters.

## API Endpoints

### 1. List StorageClasses
**GET** `/api/v1/storage/storageclass`

Lists StorageClass declarations managed by RLark from the management plane. It does not query Agents or verify the live state of data-plane resources.

#### Query Parameters
- `clusters` (optional): A comma-separated list of cluster IDs, for example `?clusters=agent-beijing,agent-shanghai`. When supplied, the response includes only declarations whose configured target clusters intersect with the specified IDs. If omitted, all managed declarations are returned.

#### Example Response
```json
{
  "data": {
    "ceph-rbd": {
      "name": "ceph-rbd",
      "storageClassName": "sc-0123456789abcdef",
      "clusters": ["agent-beijing", "agent-shanghai"],
      "description": "Ceph RBD StorageClass",
      "bucket": "",
      "provider": "s3",
      "endpoint": "https://s3.amazonaws.com",
      "region": "us-east-1",
      "pathStyle": false,
      "accessKeyId": "AKIA..."
    }
  },
  "success": true
}
```

| Field | Type | Description |
|------|------|------|
| `name` | `string` | Human-readable StorageClass declaration name |
| `storageClassName` | `string` | Generated `sc-<id>` Kubernetes StorageClass name deployed to target data-plane clusters; use this value in PVC `spec.storageClassName` |
| `clusters` | `[]string` | Configured target cluster IDs for this StorageClass |
| `description` | `string` | Human-readable description |
| `bucket` | `string` | Bucket name |
| `provider` | `string` | Storage provider type, such as s3, gcs, or azureblob |
| `endpoint` | `string` | Storage service endpoint |
| `region` | `string` | Storage region |
| `pathStyle` | `bool` | Whether path-style addressing is used |
| `accessKeyId` | `string` | Masked Access Key ID (`<first two characters>*****<last two characters>`); the Access Key Secret is never returned |

### 2. List Storage Providers
**GET** `/api/v1/storage/storageclass/provider`

Lists the supported storage providers. Any authenticated user can read this catalog and create, update, or delete StorageClasses. The catalog is the single source of truth for the `provider` values accepted when creating or updating a StorageClass: submit `value`, and use `name` for display only. rclone does not reject an unknown provider — it logs a warning and falls back to its generic profile — so the Gateway rejects any `provider` that is not in this catalog.

#### Example Response
```json
{
  "data": [
    { "name": "AWS S3", "value": "AWS" },
    { "name": "阿里云 OSS", "value": "Alibaba" },
    { "name": "MinIO", "value": "Minio" }
  ],
  "success": true
}
```

### 3. Create a StorageClass
**POST** `/api/v1/storage/storageclass`

Creates a StorageClass declaration in the management plane. Replication and Delivery controllers asynchronously distribute the generated rclone CSI StorageClass and its Secret to the requested target clusters.

The Gateway normalizes and validates the S3 configuration before storing it:

- `provider` must match a catalog entry. The canonical value, the display name, and the legacy spellings written by older clients are all accepted, and the record is always stored as the canonical value. An unrecognized value is rejected with `400`.
- `endpoint` must be a bucket-free `http`/`https` URL; a missing scheme defaults to `https`. The bucket name is never embedded in the endpoint because both the Gateway S3 client and rclone compose virtual-hosted URLs themselves.
- `endpoint` may be left empty only for providers that publish a regional endpoint format, such as Alibaba Cloud OSS, in which case `region` is required and the endpoint is derived from it.
- `path_style` is forced to `false` for providers that only answer virtual-hosted requests, such as Alibaba Cloud OSS, Tencent COS, and Huawei OBS. Otherwise the submitted value is kept.
- After structural validation, the Gateway performs an authenticated `HeadBucket` request with a bounded timeout. Create and update requests are rejected with `400 INVALID_REQUEST` when the configured credentials cannot access the bucket. The response does not include provider error details or credentials.

Existing records written before this validation existed are canonicalized on read, so the Gateway S3 client and the rclone config delivered to the data plane always agree on the provider and the addressing style.

Validation failures use stable machine-readable error codes and non-sensitive structured details. In particular, omitted required fields return `400` with code `STORAGE_CLASS_REQUIRED_FIELDS` and `details.fields`, for example:

```json
{
  "code": "STORAGE_CLASS_REQUIRED_FIELDS",
  "message": "bucket are required",
  "details": { "fields": ["bucket"] }
}
```

Clients must use `code` and `details.fields` for localized field feedback instead of translating the human-readable `message`.

#### SMB (NAS) StorageClasses

Set `type` to `smb` to mount an SMB/CIFS share instead of object storage. `type` defaults to `s3`, and records written before this field existed are read as `s3`. SMB classes use the same rclone CSI driver (`rclone.csi.veloxpack.io`) through rclone's `smb` backend, so no additional driver is required. Example request:

```json
{
  "name": "nas-post-train",
  "clusters": ["rlark-local-dev-gpu"],
  "type": "smb",
  "host": "10.0.0.10",
  "share": "shared",
  "sub_path": "team/data",
  "username": "user",
  "password": "********",
  "domain": "corp.example.com"
}
```

| Field | Required | Notes |
|-------|----------|-------|
| `host` | Yes | Hostname or IP only; a leading `//` or `smb://` is stripped. Ports and paths are rejected. |
| `share` | Yes | A single share name. |
| `sub_path` | No | Directory inside the share to mount; leading and trailing `/` are trimmed and `.`/`..` segments are rejected. |
| `username` | Yes | Returned in list responses; it is not treated as secret. |
| `password` | Yes on create | Never returned. Stored in the control-plane declaration and rendered into the data-plane rclone config in rclone's obscured form. Obscuring is not encryption; protect the Secrets accordingly. |
| `domain` | No | Windows/AD domain. |

For SMB classes the S3 fields (`provider`, `endpoint`, `bucket`, `region`, `path_style`, access keys) are ignored and the provider catalog does not apply. The StorageClass is rendered with `remote: smb` and `remotePath: <share>/<sub_path>`. Validation only checks that TCP port 445 on `host` is reachable within the timeout; the Gateway does not log in to the share, so wrong credentials surface when a Pod mounts the volume. The file browser routes (sections 6–9) return `400` for SMB classes.

### 4. Update a StorageClass
**PUT** `/api/v1/storage/storageclass/{name}`

Updates the object storage configuration and target clusters for the specified StorageClass declaration. The `clusters` field is the desired final target-cluster set; the distribution workflow cleans up resources from clusters omitted from that set. During an update, `access_key_id` and `access_key_secret` may be left empty; the Gateway reads and reuses the corresponding stored values, so credentials are not replaced with empty values. A listed `accessKeyId` is masked with five asterisks; clients must treat any submitted `access_key_id` containing `*****` as unchanged rather than as a replacement. When credentials are retained, the Gateway validates the updated connection with the previously stored Access Key ID and secret. For SMB classes, an empty `password` keeps the stored password. The storage `type` cannot be changed on update; create a new StorageClass instead.

### 5. Delete a StorageClass
**DELETE** `/api/v1/storage/storageclass/{name}`

Deletes the StorageClass declaration. Distribution finalizers asynchronously remove its Delivery declarations and generated StorageClass and Secret from all associated clusters.

### 6. List Bucket Files
**GET** `/api/v1/storage/storageclass/{name}/list`

Lists files in the StorageClass bucket. The Gateway reads the bucket directly from the control-plane StorageClass declaration.

#### Path Parameters
- `name`: StorageClass name

#### Query Parameters
- `prefix` (optional): Object-key prefix to list. A non-root directory prefix is slash-separated and ends with `/`, for example `a/b/c/`.
- `delimiter` (optional): Object-key delimiter. Defaults to `/`; with this delimiter, immediate child directories are returned in `common_prefixes`.
- `maxKeys` (optional): Maximum number of objects and common prefixes to return.

#### Directory Navigation

Object storage has prefixes rather than real directories. The file browser treats a non-empty directory prefix as a canonical slash-terminated value. For example, the directory path `a/b/c` is represented as `a/b/c/`; its parent is `a/b/`, and breadcrumbs target `a/`, `a/b/`, and `a/b/c/`. This navigation remains available when a prefix contains no direct objects or child prefixes.

#### Example Response
```json
{
  "data": [
    {"name": "model-checkpoint.pt", "size": 1048576, "modified": "2026-07-30T10:00:00Z"},
    {"name": "logs/training.log", "size": 2048, "modified": "2026-07-30T09:30:00Z"}
  ],
  "success": true
}
```

### 7. Upload a File
**POST** `/api/v1/storage/storageclass/{name}/upload`

Uploads a file to the specified StorageClass bucket using multipart/form-data.

#### Path Parameters
- `name`: StorageClass name

#### Request Body
Use multipart/form-data with the uploaded file in the `file` field.

#### Example Response
```json
{
  "data": {"key": "model-checkpoint.pt", "size": 1048576},
  "success": true
}
```

### 8. Download a File
**GET** `/api/v1/storage/storageclass/{name}/object/*key`

Downloads an object from the specified bucket and returns the raw file content.

#### Path Parameters
- `name`: StorageClass name
- `key`: Object path, such as `model-checkpoint.pt` or `logs/training.log`

#### Response
- `200`: File content as a binary stream
- `404`: File not found

### 9. Delete a File
**DELETE** `/api/v1/storage/storageclass/{name}/object/*key`

Deletes an object from the specified bucket.

#### Path Parameters
- `name`: StorageClass name
- `key`: Object path

#### Example Response
```json
{
  "data": {"deleted": true},
  "success": true
}
```

## Implementation Notes

### Control-plane Distribution Architecture

```
Gateway ──▶ Management-plane StorageClass declaration
                         │
                         ▼
              Replication Controller ──▶ Delivery Controller ──▶ K8s API
```

1. The Gateway validates a StorageClass request and stores a replication declaration in the management plane.
2. The Replication Controller creates Delivery declarations for the configured target clusters.
3. Each Delivery Controller applies the generated rclone CSI Secret and cluster-scoped StorageClass to its local Kubernetes cluster.
4. Updates replace the desired target-cluster set; deletion triggers asynchronous cleanup through distribution finalizers.

A listed declaration represents configured distribution intent. Before provisioning a PVC, ensure delivery has completed and the target cluster has a working rclone CSI driver, valid object-storage credentials, and network connectivity to the storage service.

### File Structure
- **`../pkg/gateway/storage_handler.go`** - Storage API handlers and generated manifests
- **`../pkg/controllermanager/replication/controller.go`** - Control-plane replication controller
- **`../pkg/agent/controllers/delivery/controller.go`** - Data-plane delivery controller
- **`../pkg/gateway/router.go`** - Route registration

### Data Structures
```go
type StorageClassData struct {
    Name        string   `json:"name"`
    Clusters    []string `json:"clusters"`
    Description string   `json:"description"`
    Bucket      string   `json:"bucket"`
    Provider    string   `json:"provider"`
    Endpoint    string   `json:"endpoint"`
    Region      string   `json:"region"`
    PathStyle   bool     `json:"pathStyle"`
}

type Provider struct {
    Name  string `json:"name"`
    Value string `json:"value"`
}
```

## Usage Examples

### List All Managed StorageClasses
```bash
curl "http://localhost:8080/api/v1/storage/storageclass"
```

### List StorageClasses Targeting a Specific Cluster
```bash
curl "http://localhost:8080/api/v1/storage/storageclass?clusters=agent-beijing"
```

### List Storage Providers
```bash
curl "http://localhost:8080/api/v1/storage/storageclass/provider"
```

## Integration with Task PVC Mounts

A Task declares remote storage through a Kubernetes generic ephemeral volume:

```yaml
workload:
  template:
    spec:
      volumes:
        - name: data
          ephemeral:
            volumeClaimTemplate:
              spec:
                accessModes: [ReadWriteOnce]
                storageClassName: ceph-rbd
                resources:
                  requests:
                    storage: 10Gi
```

Kubernetes creates a PVC for each Pod from `volumeClaimTemplate` and removes it with the Pod. The current Web UI writes this generic ephemeral-volume form and uses the Storage API to retrieve RLark-managed StorageClass declarations for the selected cluster. `pvcStorageMap` and `pvcSizeGbMap` are not part of the canonical Task schema; old resources that depend on those fields must be replaced during a breaking upgrade.

## Mount endpoint override

The optional S3 field `mount_endpoint` is accepted by create/update and returned by list. `endpoint` remains the address used by Gateway credential validation (`HeadBucket`) and file APIs; CSI Secret generation uses `mount_endpoint` when nonempty. Omitted or empty values use `endpoint`; sending an empty value on update clears the override. Both addresses use the same provider, bucket, region, credentials and path-style setting, and the same endpoint URL normalization. SMB rejects a nonempty `mount_endpoint`.

For a cloud-specific storage class, keep a public Gateway endpoint and set a cloud-internal mount endpoint. Gateway does not probe the mount endpoint: verify DNS, TLS, credentials and actual mounting from the target cluster. Configuration success and listing do not establish delivery or mount readiness. All clusters targeted by this declaration receive the same mount endpoint; create separate declarations when clusters need different addresses. Existing mounted volumes are not guaranteed to reload a changed endpoint; validate with a new test Pod before migrating workloads.

```json
{
  "name": "ks3-post-train-ksyun",
  "type": "s3",
  "clusters": ["rlark-ksyun-gpu"],
  "provider": "Other",
  "endpoint": "https://ks3-cn-ningxia.ksyuncs.com",
  "mount_endpoint": "https://ks3-cn-ningxia-internal.ksyuncs.com",
  "bucket": "stardust",
  "region": "cn-ningxia",
  "path_style": false
}
```

This is an environment-specific configuration example, not a ready-to-submit request: supply authorized credentials through a protected channel. Do not include credentials in source control or logs. Delivery remains asynchronous; confirm the generated `sc-<id>` in the target cluster before using it in a Job.

## S3 prefix and VFS cache mode

S3 declarations accept optional `prefix` and `vfs_cache_mode` fields on create/update and return them from list. `prefix` is a slash-separated path within the bucket and becomes the mounted root below the bucket; for example, bucket `stardust` and prefix `ks3-regular` generate rclone `remotePath: stardust/ks3-regular`. Empty prefix mounts the bucket root. `vfs_cache_mode` accepts `writes` or `off`; create requests without it use `writes`; update requests without it retain the stored mode. The setting applies only to the generated CSI StorageClass mount options. The prefix scopes the CSI mount only; Gateway file APIs still address bucket-relative keys, so it is not an authorization boundary. SMB rejects these S3-only fields.

For large outputs that must be visible in object storage when a file is closed, use a dedicated class with `vfs_cache_mode: off`. With cache disabled, remote upload errors can be returned from file operations such as close; applications must propagate those errors and retry failed files. This setting does not provide application-level atomic publication or replace end-to-end failure testing.

```json
{
  "name": "fleet-ks3",
  "type": "s3",
  "clusters": ["rlark-ksyun-gpu"],
  "provider": "Other",
  "endpoint": "https://ks3-cn-ningxia.ksyuncs.com",
  "bucket": "stardust",
  "region": "cn-ningxia",
  "path_style": false,
  "prefix": "ks3-regular",
  "vfs_cache_mode": "off"
}
```

The Gateway validates bucket access against the configured endpoint with `HeadBucket`. A 403 is an authorization response and does not by itself indicate that the endpoint is unreachable; check the credential, bucket policy and required permissions. Never put credentials in source control, examples, or logs.


### KS3 multipart acceptance limitation

Cache mode `off` is not a provider compatibility guarantee. In a tested KS3 configuration with CSI rclone v0.2.0 (embedded rclone v1.71.2), a completed multipart object's 32-hex ETag differs from content MD5. Streaming close can return EIO (`corrupted on transfer`) despite independently verified object bytes. Do not disable integrity checks to accept such a mount. Validate the driver/provider combination before using it for archival. Unknown-size streams also require a suitable multipart chunk size: the default 5 MiB and 10,000-part limit allow only about 48.8 GiB. The current storage form does not expose chunk sizing; large-checkpoint acceptance requires a compatible driver/configuration and real failure/recovery testing. Separate mounts have independent directory caches, so close success alone does not promise immediate visibility through another mount.


## Per-storage S3 mount profiles

Create/update accepts `mount_profile` (`default`, `read`, `archive`) and `mount_options` (an array of `key=value` strings). List returns these plus `effective_mount_options` and `effective_backend_options`. Omitted update fields retain their stored values; an empty options array clears overrides. Default preserves existing settings. Read enforces a read-only mount with a five-minute directory cache. Archive enforces write cache off, a five-second directory cache, 32 MiB S3 upload chunks and four concurrent upload parts. This is a configuration profile, not a successful KS3 archival acceptance claim.

Explicit options override profile defaults. Duplicate/unknown keys and conflicts with `vfs_cache_mode` or mandatory profile settings are rejected. Supported keys: `vfs-cache-mode`, `read-only`, `fast-list`, `dir-cache-time`, `attr-timeout`, `poll-interval`, `vfs-write-back`, `vfs-cache-max-age`, `buffer-size`, `transfers`, `checkers`, `low-level-retries`, `tpslimit`, `tpslimit-burst`, `s3-chunk-size`, `s3-upload-concurrency`. Backend options are rendered as `chunk_size` / `upload_concurrency` in the remote configuration. Arbitrary credentials, paths, control endpoints and `ignore-checksum` cannot be supplied here. SMB does not accept S3 profiles or overrides.

Each generated S3 StorageClass uses a stable class-specific rclone remote alias (`s3-<storage-class-id>`) in both `remote` and `configData`. This prevents read and archive classes on the same node plugin from sharing mutable rclone configuration. It isolates configuration only; it does not repair provider checksum semantics.

Profiles do not set an account-wide request budget. Confirm the source and scope of 429 responses before choosing TPS limits across Fleet, DataTools and the image registry; CSI and Gateway have separate request paths. A short directory cache is not an atomic publication or visibility guarantee. Updates apply to newly mounted volumes: stop/update/start the affected Job only in its maintenance window. Changing a storage does not restart active Jobs automatically.
