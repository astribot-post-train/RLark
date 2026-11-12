# Fleet NAS path contract

Effective convention: 2026-11-12. This document fixes paths for future development; migration observations and cross-site acceptance are separate. It does not claim all Jobs have switched. [中文](fleet-storage-paths.zh-CN.md).

## Canonical paths and mounts

The NAS share is `//10.11.5.12/post-train_group`, mounted at `/mnt/post_train` on hosts. Containers use the same full absolute paths. Do not introduce `/fleet`, `/mnt/rl`, or compatibility symlinks.

| Purpose | Identical host and container path | Access |
| --- | --- | --- |
| Personal Fleet source | `/mnt/post_train/RL/user/<name>/fleet` | Developer writes; Jobs read only |
| Liangcai source | `/mnt/post_train/RL/user/liangcai/fleet` | Working directory; append `/src` for `PYTHONPATH` |
| Input datasets | `/mnt/post_train/RL/fleet-data/datasets` | Read only by default |
| Models and tokenizer | `/mnt/post_train/RL/fleet-data/models` | Read only by default |
| Collection output | `/mnt/post_train/RL/fleet-data/collections/<run-id>` | Owning Run only |
| Checkpoints | `/mnt/post_train/RL/fleet-data/checkpoints/<run-id>` | Owning Run only |
| Published files | `/mnt/post_train/RL/fleet-data/artifacts/<artifact-id>` | Immutable after publication |
| Incomplete writes | `/mnt/post_train/RL/fleet-data/staging/<run-id>` | Owning Run only |
| Migration/checksum manifests | `/mnt/post_train/RL/manifests` | Operator writes; no credentials |

A run ID identifies one execution. A new execution needs a new ID; reusable Job names are insufficient. Existing KPFS and older NAS datasets stay at their original locations unless explicitly inventoried for migration. Do not automatically copy, delete or symlink external data. Directory names are not authorization boundaries; verify NAS ACLs and actual workload identities at each site.

## Job configuration

Images provide Python, Torch, Ray, RLinf and system dependencies. Source location belongs to Job configuration, not the image. The following Pod-template fragment belongs inside the RLark Job Kubernetes workload. Use an immutable digest from the existing private registry; this change does not use GHCR.

```yaml
spec:
  containers:
    - name: main
      workingDir: /mnt/post_train/RL/user/liangcai/fleet
      env:
        - name: PYTHONPATH
          value: /mnt/post_train/RL/user/liangcai/fleet/src
        - name: FLEET_FRONTEND_ROOT
          value: /mnt/post_train/RL/user/liangcai/fleet/frontend/dist
        - name: RLINF_COLLECTION_ROOT
          value: /mnt/post_train/RL/fleet-data/collections/run-20261112-example
      volumeMounts:
        - {name: source, mountPath: /mnt/post_train/RL/user/liangcai/fleet, readOnly: true}
        - {name: models, mountPath: /mnt/post_train/RL/fleet-data/models, readOnly: true}
        - {name: datasets, mountPath: /mnt/post_train/RL/fleet-data/datasets, readOnly: true}
        - {name: collection, mountPath: /mnt/post_train/RL/fleet-data/collections/run-20261112-example}
  volumes:
    - name: source
      hostPath: {path: /mnt/post_train/RL/user/liangcai/fleet, type: Directory}
    - name: models
      hostPath: {path: /mnt/post_train/RL/fleet-data/models, type: Directory}
    - name: datasets
      hostPath: {path: /mnt/post_train/RL/fleet-data/datasets, type: Directory}
    - name: collection
      hostPath: {path: /mnt/post_train/RL/fleet-data/collections/run-20261112-example, type: Directory}
```

Operators must pre-create the example Run directory and replace every corresponding path for a new Run. Mount only required subdirectories; checkpoints, staging and artifacts follow the same rule. On the selected node, verify `findmnt -T /mnt/post_train` identifies the expected CIFS mount. `Directory` checks existence only: an absent NAS may leave an ordinary local directory. Startup must inspect `/proc/self/mountinfo` for the actual filesystem (`cifs` for native SMB; separately validate CSI) and read the source before running, failing closed otherwise. Mid-run NAS failures still require timeouts and recovery.

Do not put virtualenvs, site-packages, caches, JWT/database credentials or PostgreSQL PGDATA/WAL in shared source. Caches remain node-local and instance-scoped. Build frontend assets locally before publishing them with source. A source snapshot does not prove runtime dependency compatibility.

## Consistency and migration

Developers may edit the canonical source entry. Freeze it for active Jobs and record commit, dirty state, diff digest, configuration and image digest. Do not edit while modules are loading. Production snapshots use their own full paths with identical host/container mounts, never short aliases.

Reference files using `storage_id + relative_path`, separate from site mount mappings and business identity. Fleet/DataTools still need implementation of that metadata contract; this path change does not expose it as a completed interface.

Write outputs to staging, validate counts, sizes and SHA-256, confirm readability from another client, then publish artifacts and completion manifests. A direct SMB mount may use same-filesystem rename for directory publication. Do not infer cross-site atomicity or durability from rclone rename, close or flock. File publication and PostgreSQL commit are separate recoverable steps using staging / committed / failed; never replay robot actions whose outcome is unknown.

Inventory source, revisions and local modifications first. Prepare a complete checkout, built assets and checksum manifest in a new staging directory. Conflicting target content is not overwritten. After the user's decision and confirmation that no active task uses the entry, rename the old directory to a timestamped backup and move the prepared checkout into place. The entry can briefly be absent between renames, so readers must be stopped. Restore the old directory if switching fails. Keep the backup and manifests; do not delete datasets, annotations or older data.

The database target is an independent Fleet PostgreSQL service in the cloud, sharing catalog and annotation tables with DataTools; this supersedes the earlier proposal to reuse an existing instance. PGDATA belongs on cloud persistent disk, not NAS/KS3. This task fixes file paths only, does not migrate databases, and leaves existing local RLark/Station PostgreSQL unchanged.

## Acceptance and status

- Verify identical read-only mounts, Python module origin and source SHA-256 independently on local, robot and cloud nodes. Mark untested sites as pending.
- With isolated Run data, test permissions, cross-client checksums, interrupted staging, NAS disconnects and Pod/node restart. Do not change real data or robot state.
- Missing storage must fail startup, never create a silent local substitute. rclone `dir-cache-time=5m` is not an end-to-end visibility bound.
- Compare inventories, Git HEAD, dirty/untracked files and frontend digests across the switch; preserve rollback.
- Documentation, examples and prepared source do not establish cross-site runtime acceptance. This task does not automatically submit or restart existing Jobs.

## Local execution record: 2026-11-12

- The fixed source entry now contains local Fleet `bfc5aad`, including uncommitted/untracked source changes and rebuilt frontend assets: 231 published files, 8,337,599 bytes, all verified by SHA-256. Virtualenvs, credentials, caches, output and draft folders were excluded.
- The previous `832a8b9` checkout, including its untracked files, is retained at `/mnt/post_train/RL/user/liangcai/fleet.backup-20261112T054328Z`.
- `/mnt/post_train/RL/manifests/fleet-nas-paths-20261112T054328Z/migration.json` records the migration. Adjacent files retain source Git status/diff and previous status.
- Created empty datasets, models, collections, checkpoints, artifacts and staging directories under fleet-data. Older NAS/KPFS large files have not moved. ACLs, networking, databases and existing Jobs were not changed.
- Before switching, checked active Pods at all three sites and management Job/Task references. Cloud DataTools' `/mnt` does not expose this NAS source. NAS Python parsing/import origin, identical-path Pod-template validation and read-only input checks passed, along with Fleet frontend typecheck/build and both strict documentation builds.
- Cross-site workload reads/writes, disconnects and node restart recovery remain pending; local checks do not establish these capabilities.
