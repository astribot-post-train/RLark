# Storage

## Storage Types

The Job editor can create two storage choices: a host directory or a PVC-backed storage mount. A Job submitted through the API may also use other Kubernetes-native PodTemplate volumes, which the editor shows read-only. Kubernetes-native PodTemplate volumes define their lifecycle:

| Type | Use Case | Lifecycle |
|------|----------|-----------|
| Host Directory | Data already on the node, high I/O | Job lifecycle actions do not delete data |
| Object Storage (Pod-scoped ephemeral PVC) | The form written by the current Web UI for a PVC-backed mount | Kubernetes creates and removes the PVC with its Pod |
| Existing persistent PVC reference | A Task volume uses `persistentVolumeClaim.claimName` | RLark neither creates nor deletes the referenced PVC; its administrator controls the lifecycle |
| Other Kubernetes-native volume (`emptyDir`, `configMap`, `secret`, and so on) | Submitted through the API; the Job editor only displays it | The volume type defines it; RLark writes back the original definition when a Job is edited or cloned |

## Host Directory

- Administrator must confirm the path exists on the target node with correct permissions
- Specify source path (node filesystem) and mount path (container filesystem)
- Data persists on the node after job deletion

## Object Storage

- Uses a Kubernetes generic ephemeral volume and an RLark-managed StorageClass
- The current Web UI writes PVC-backed mounts as `ephemeral.volumeClaimTemplate`; newly created or edited Jobs therefore use Pod-scoped ephemeral PVCs
- Each PVC mount defaults to a 10Gi request; the configurable size range is 1–200Gi
- Select a cluster first; the dropdown lists StorageClass declarations configured to target that cluster
- Each worker Pod receives its own PVC

## SMB / NAS Shares

- Choose **SMB / NAS share** as the storage type when creating a storage class, then enter the server, share name, optional sub path, username, password, and optional domain
- Jobs select an SMB storage class exactly like an object-storage class; the PVC mounts the share (or its sub path) through the rclone CSI driver
- Creation only checks that the server's port 445 is reachable. Wrong credentials are reported when a task mounts the volume, so check the Pod events if a mount fails
- SMB storage classes do not support online file browsing
- Leave the password empty when editing to keep the stored password; the storage type cannot be changed after creation

## Managing Storage Classes

Open **Storage** to review object-storage and SMB configurations, target clusters, providers, and locations (a bucket, or `//server/share/path` for SMB). Creating or updating a storage class stores its declaration in the control plane; the generated rclone CSI StorageClass and Secret are then distributed asynchronously to target clusters. Create the required storage class and wait for delivery before configuring a PVC mount for a Worker.

Before using a storage class, confirm the target cluster has the rclone CSI driver installed and can reach the object storage service. If a PVC remains `Pending`, check the distribution status, driver, credentials, and network connectivity.

> **Screenshot note:** Screenshots are from an example environment. Resource names and data are illustrative; your environment will differ.

![Storage management](../images/ui/storage-file-browser.png)

## Using Storage in a Training Job

When creating a job, in the Worker configuration step:
1. Select the storage type (hostPath or PVC)
2. Enter the source path (for hostPath) or select StorageClass (for PVC)
3. Enter the container mount path
4. Your training code reads/writes to the mount path

## Checking Read/Write

Verify the storage chain:
1. Source location is accessible
2. Container mount is correct
3. Application can read input data
4. Application can write output data
5. For a current Web UI Job, treat the PVC as Pod-scoped ephemeral storage and copy required output elsewhere before stopping or restarting. In `spec.workload.template.spec.volumes`, `ephemeral.volumeClaimTemplate` is Pod-scoped, while `persistentVolumeClaim.claimName` references an existing, externally managed PVC.

## Lifecycle

- Existing `persistentVolumeClaim`: RLark does not create, adopt, mutate, or delete the referenced PVC. Its lifecycle and data retention are external to the Task.
- Pod-scoped `ephemeral.volumeClaimTemplate`: deleting a Worker Pod deletes its ephemeral PVC; a new Pod receives a new PVC.
- HostPath: lifecycle actions do not delete node data.
- Other Kubernetes-native volumes: the Job editor cannot create or modify them. Editing or cloning a Job writes the original volume definition back unchanged, so the volume type keeps its own lifecycle.

An existing PVC reference must include `claimName`, and the claim must already exist in the workload namespace. Kubernetes reports mount or binding failures through the workload and Pod status. Deleting a referenced PVC is an external administrative action; confirm that no workload uses it and account for the bound PV's reclaim policy.

`pvcStorageMap` and `pvcSizeGbMap` are not part of the canonical Task schema. The current Web UI and Kubernetes-native examples write `ephemeral.volumeClaimTemplate`; old resources that depend on removed fields must be replaced during a breaking upgrade.

## API Equivalent

Use StorageClass, provider, and object-file endpoints described in the [Storage API](../storage-api.md).

## Mount endpoint

An S3 storage class can specify an optional mount endpoint. Empty values use Endpoint; an override only affects target-cluster CSI mounts, while Gateway validation and file browsing retain Endpoint. This supports Gateway public access and cloud Job internal access to the same bucket. URL validation does not establish cloud connectivity: verify with a read-only test Job. All targets share this override; use separate storage classes for different networks. Clearing the field on edit restores Endpoint. SMB does not offer this option.

S3 classes can also scope a mount to an object `prefix` inside the bucket and select the rclone VFS cache mode (`writes` or `off`). Existing classes default to `writes`. Use a separate class for workloads that require cache-disabled writes; verify close-error handling and retries on the actual target cluster before production use. See the [Storage API](../storage-api.md) for the generated `remotePath` behavior.


### KS3 multipart acceptance limitation

Cache mode `off` is not a provider compatibility guarantee. In a tested KS3 configuration with CSI rclone v0.2.0 (embedded rclone v1.71.2), a completed multipart object's 32-hex ETag differs from content MD5. Streaming close can return EIO (`corrupted on transfer`) despite independently verified object bytes. Do not disable integrity checks to accept such a mount. Validate the driver/provider combination before using it for archival. Unknown-size streams also require a suitable multipart chunk size: the default 5 MiB and 10,000-part limit allow only about 48.8 GiB. The current storage form does not expose chunk sizing; large-checkpoint acceptance requires a compatible driver/configuration and real failure/recovery testing. Separate mounts have independent directory caches, so close success alone does not promise immediate visibility through another mount.


## Per-storage S3 mount profiles

Create/update accepts `mount_profile` (`default`, `read`, `archive`) and `mount_options` (an array of `key=value` strings). List returns these plus `effective_mount_options` and `effective_backend_options`. Omitted update fields retain their stored values; an empty options array clears overrides. Default preserves existing settings. Read enforces a read-only mount with a five-minute directory cache. Archive enforces write cache off, a five-second directory cache, 32 MiB S3 upload chunks and four concurrent upload parts. This is a configuration profile, not a successful KS3 archival acceptance claim.

Explicit options override profile defaults. Duplicate/unknown keys and conflicts with `vfs_cache_mode` or mandatory profile settings are rejected. Supported keys: `vfs-cache-mode`, `read-only`, `fast-list`, `dir-cache-time`, `attr-timeout`, `poll-interval`, `vfs-write-back`, `vfs-cache-max-age`, `buffer-size`, `transfers`, `checkers`, `low-level-retries`, `tpslimit`, `tpslimit-burst`, `s3-chunk-size`, `s3-upload-concurrency`. Backend options are rendered as `chunk_size` / `upload_concurrency` in the remote configuration. Arbitrary credentials, paths, control endpoints and `ignore-checksum` cannot be supplied here. SMB does not accept S3 profiles or overrides.

Profiles do not set an account-wide request budget. Confirm the source and scope of 429 responses before choosing TPS limits across Fleet, DataTools and the image registry; CSI and Gateway have separate request paths. A short directory cache is not an atomic publication or visibility guarantee. Updates apply to newly mounted volumes: stop/update/start the affected Job only in its maintenance window. Changing a storage does not restart active Jobs automatically.
