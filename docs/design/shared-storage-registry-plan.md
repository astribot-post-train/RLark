# Shared Storage and Registry: Design and Operations

## KS3 access from Kingsoft Cloud Jobs: read-only investigation

On 2026-11-12, the RLark `ks3-post-train` declaration used bucket `stardust` and endpoint `https://ks3-cn-ningxia.ksyuncs.com`. This is the Gateway endpoint. The existing cloud declaration separately uses `https://ks3-cn-ningxia-internal.ksyuncs.com` for CSI mounts; the prior isolated reads validated that pair. A new direct probe of the public bucket hostname from `10-0-2-196` still timed out after 5 seconds. The new class therefore inherits the existing validated endpoint pair. A prior HTTP 403 is an authorization response and does not prove endpoint reachability failure. StorageClass creation still requires credentials that pass Gateway `HeadBucket` validation.

Read-only inspection on the bare-metal hosts confirmed both Kubernetes nodes are Ready. The rclone CSI Node DaemonSet currently selects only `kubernetes.io/hostname=10-0-2-196` and has one ready Pod. It already tolerates all taints with `operator: Exists`; the hostname selector, rather than a missing control-plane toleration, excludes `10-0-2-57`. The RLark-managed Addon should be updated to remove the Node selector, then both node registrations must be verified. No live Addon or StorageClass change is recorded as completed by this investigation.

The Fleet-specific class is planned as a separate declaration using bucket `stardust`, prefix `ks3-regular`, virtual-host addressing, and VFS cache mode `off`. Its generated `remotePath` should be `stardust/ks3-regular`, matching `ks3://stardust/ks3-regular/`; the existing class remains unchanged. The cache-disabled close and interrupted-upload retry path still requires validation on a real mount before any Fleet Run switches.

The Gateway now gives each generated S3 StorageClass a stable class-specific rclone remote alias (`s3-<storage-class-id>`) and uses that alias consistently in the CSI parameters and config section. This prevents separate read/archive profiles from sharing mutable remote configuration in one node plugin. It does not resolve the KS3 multipart ETag/CRC64 compatibility failure; the CSI driver still needs a provider-aware integrity fix and real acceptance tests.

> Update 2026-11-12: Fleet now uses the [canonical full NAS paths](fleet-storage-paths.md). The personal Fleet KS3 tree below is a historical proposal, not the convention for new Fleet Jobs. KS3 data and registry pilot observations remain historical evidence.

This document records the environment-specific design, observed progress and manual operating procedures. It is not a generally supported RLark feature or a claim that the full deployment is complete. Status is a snapshot, not an ongoing health check.

### Storage class roles and retention

Keep the three declarations separate until the KS3 multipart compatibility issue is resolved:

| Declaration | Scope | Current use | Decision |
| --- | --- | --- | --- |
| `ks3-post-train` | Local and robot clusters | Existing shared KS3 reads and mounts | Keep |
| `ks3-post-train-ksyun` | `rlark-ksyun-gpu` only | Cloud read-only jobs, model/data reads and validation | Keep |
| `fleet-ks3-ksyun` | `rlark-ksyun-gpu` only, prefix `ks3-regular` | Fleet archive candidate with cache disabled | Keep for isolated testing; not accepted for active Runs |

The two `ks3-post-train` declarations are not duplicates: the first preserves the local/robot endpoint and delivery scope, while the `-ksyun` declaration uses the cloud CSI endpoint. Do not delete either until all PVC, Job and validation references have been checked. `fleet-ks3-ksyun` must not replace the cloud read class while close still returns EIO.

## Architecture and decisions

```text
                              KS3: bucket stardust, Ningxia
                         ks3-regular/post_train/  (shared data)
                           /          |           \
                          /           |            \
                 KS3FS, manual   cloud access      KS3FS, manual
                   verified       not verified       verified
                     /                |                 \
        Local development host    Cloud A/B             Robot x86
        RLark services + UI       compute/jobs          workloads
        (existing local setup)    (future validation)   (future validation)
                     |
        Independent local OCI registry (limited pilot, TLS + authentication)
                     |
        KS3 object storage backend (pilot push and integrity verified)
```

- **Code:** aim to keep Cailiang's RLark and Fleet development trees under a personal KS3 prefix; this is not yet a migrated or validated development workflow. Initially allow only one writer. If editor, Git, build, locking or cache behavior is unsuitable, keep local working trees and publish versioned artifacts instead.
- **Data and jobs:** share existing datasets through the parent mount; place new shared datasets and each job run's inputs and outputs under distinct approved prefixes. A run publishes a completion marker only after its outputs are closed. Avoid concurrent writers to the same files.
- **Images:** an independent local Distribution registry is running as a limited pilot with a KS3 object API backend, not a KS3FS directory. Local Docker push, restart and robot Docker and Kubernetes pulls have been tested. RLark already has an administrator credential management page; this pilot's credential is distributed to the selected development and robot clusters. The actual RLark Job path, cloud connectivity, production security and recovery remain unverified. Do not reuse or alter the existing unauthenticated local registry without separately reviewing its users and security.
- **Database:** keep the live PostgreSQL database on an appropriate local filesystem or Docker volume, not on KS3FS. A separate KS3 backup location, retention policy and tested restore are planned, not configured by this work.
- **Security:** directory names are not access controls. Use cloud policies and host permissions; never store access keys in Git, logs, manifests or screenshots. TLS, registry authentication, least-privilege storage credentials and access from each client are acceptance requirements.

KS3FS presents object storage as files; do not assume POSIX locking, atomic directory rename, cache coherence or safe multi-writer Git operations. Validate actual application behavior and failure recovery before relying on it. The parent mount is read-write: existing datasets and models are not technically protected from writes.

## Agreed and proposed paths

The agreed mount maps `/mnt/ks3/post_train/` to `ks3://stardust/ks3-regular/post_train/` on the development host and robot. Use the same mapping on cloud hosts only after verifying their access and existing mounts. Existing datasets are read from this mount, not a separate dataset mount.

```text
/mnt/ks3/post_train/
├── datasets/                          existing shared datasets
├── models/                            existing shared models
└── rlark/                             project prefix
    ├── .validation/                   isolated validation objects
    ├── users/cailiang/code/rlark/     proposed personal RLark tree
    ├── users/cailiang/code/fleet/     proposed personal Fleet tree
    └── shared/
        ├── datasets/                  proposed new shared datasets
        └── jobs/<job-id>/<run-id>/    proposed isolated run inputs and outputs
```

The project code and shared job/dataset subdirectories above are proposals, not evidence that they exist or that code has been migrated. The pilot registry uses a separate `rlark/registry/` prefix; its production suitability is not yet established. The database backup prefix has not been chosen or provisioned. Do not overwrite or migrate existing data before inspection and approval.

## Progress snapshot: 2026-11-05

| Area | Observed status | Remaining work |
| --- | --- | --- |
| Development host | KS3FS 1.3.1 manually mounted read-write as `cailiang`; `datasets/` accessible. A validation object survived unmount and remount. | Boot recovery, fail-closed behavior, other users and container access. |
| Robot `x86` | KS3FS 1.3.1 manually mounted read-write as `astribot`; `datasets/` accessible. A user-level scoped proxy supplies the required DNS and source address. | Persistent lifecycle, network-change recovery, other users and container access. |
| Cross-host | Each host wrote and closed a unique validation object and the other read it with matching SHA-256. Objects remain under `rlark/.validation/`. | This is not a coherence, concurrent-write, large-model or restart test. |
| Cloud A/B | A's direct registry connection timed out; an authenticated TLS manifest request succeeded through a temporary SSH reverse tunnel. The user reports prior Lumo2 validation; it was not repeated here. | Validate durable, secure cloud access and an actual image pull; verify storage separately on A/B. B access was not tested. |
| Code and jobs | Personal and shared layouts are planned; no code migration or workload validation was performed in this work. | Validate Git/build behavior, then agree on any migration and run a test Job. |
| Registry and RLark | A separate TLS/authenticated pilot uses KS3; a local push, restart, robot direct Docker pull and Kubernetes Job pulls on the robot and `cailiang` succeeded. RLark delivered the credential to both selected clusters. | Configure and verify trust on other nodes; verify an actual RLark Job, cloud connectivity, least privilege and production recovery. The existing unauthenticated registry was not changed. |
| Database | Local PostgreSQL was observed using a Docker volume. | Design and test separate backup and restore. |

Local and robot `findmnt` and dataset access, and robot proxy status, were rechecked on 2026-11-05. Previous validation used unique objects only; it did not modify existing datasets or models. Mount status alone does not prove the configured remote prefix or continued remote access.

## Registry pilot: 2026-11-05

- The `rlark-ks3-registry` Docker container uses Distribution 3.1.2 pinned by digest, binds only `192.168.31.2:5443`, uses HTTPS and bcrypt htpasswd, and has `unless-stopped` restart policy. The self-signed certificate covers that IP and expires on 2027-11-05; clients need the dedicated CA, and changing the address requires coordinated certificate and client updates. The pre-existing registry on port 5555 remains separate.
- The backend is `ks3://stardust/ks3-regular/post_train/rlark/registry/`, which was empty when checked. KS3 access uses the S3 API, with redirects disabled; image downloads pass through the development host. The test image and its objects remain and incur storage costs. The registry has read-only container filesystem mounts and bounded tmpfs scratch; this does not bound the Docker image cache, uploads or KS3 costs.
- Secrets and configuration are in the owner-only development-host directory `~/.config/rlark-registry/`: `config.yml` contains KS3 credentials and the registry HTTP secret; `server.key`, `htpasswd`, and `client-password` are sensitive. Protect and back up these files; losing them or the host can make the registry inaccessible even though images remain on KS3. The KS3 credential is reused, and its actual least-privilege scope has **not** been verified. Rotation and disaster recovery are not yet tested. Do not print or commit these files.
- The htpasswd user `rlark` can both push and pull: it is **not** a read-only robot account. The same credential is distributed to `rlark-system` on the selected development and robot clusters through RLark's administrator API; anyone who can read those Secrets can also push. Revoke or replace it if that trust is unacceptable. Multi-user authorization, audit and isolation are not implemented by this pilot.
- Dedicated CA files were installed for local and robot Docker, robot Kubernetes containerd (`/etc/containerd-robot/certs.d/192.168.31.2:5443/`), and development containerd (`/etc/containerd/certs.d/192.168.31.2:5443/`); no runtime was restarted during initial installation. Robot Docker and an explicit Kubernetes Job with the delivered `imagePullSecrets` pulled successfully after the registry restart. Local Docker push succeeded; the development Kubernetes Job initially reported `x509: certificate signed by unknown authority` even after adding registry-specific trust; see the subsequent fix below. Other development nodes are not configured. Do not restart a busy runtime without coordination.
- The local and robot Kubernetes Jobs used explicit `imagePullSecrets`; this verifies credential delivery and the robot Kubernetes runtime, **not** automatic injection by the currently deployed RLark Agent or an end-to-end RLark Job. Current source includes matching injection for Kubernetes Tasks, but deployed versions and other namespaces must be checked separately. The local Gateway now provides verified read-only catalog, tag and digest browsing with a push/pull command guide; actual push and pull remain Docker client operations. It is a local pilot, not a full release or reboot acceptance. See the repository's `docs/operations/private-registry.md` for deployment and rollback details, and the limitations of the temporary cloud tunnel test below.

### Follow-up connectivity, certificate and transfer checks: 2026-11-05

- Cloud A was reached by SSH on `10001` using its existing trusted host key; its observed private address `10.1.3.214` matched the enrollment record. A direct connection to `192.168.31.2:5443` timed out after 5 seconds; the exact network cause is unknown. A temporary SSH reverse tunnel, listening only on A's loopback, then allowed A to verify the server certificate and authenticate a small-image manifest request: HTTP 200 and the digest matched the small-image record below. The CA and credential were used only in the test process and the tunnel was closed. No persistent trust or credential was installed on A; Docker, Podman and nerdctl commands were not found. This is not a cloud container-runtime pull, a persistent networking solution or a B test, and it does not establish image workload support in A's Raw runtime.
- Development node `cailiang` uses containerd 2.2.1 in transfer-pull mode; its original CRI registry `config_path` was `/etc/containerd/certs.d:/etc/docker/certs.d`. A Kubernetes Job with explicit credentials failed with `x509`. In isolated `ctr` transfer diagnostics, the same CA under the single directory `/etc/containerd/certs.d` reached the authentication stage (no credentials were supplied); the original colon-separated value reproduced `x509`. The [containerd 2.2.1 transfer source](https://github.com/containerd/containerd/blob/v2.2.1/core/transfer/registry/registry.go) treats the value as one host directory. With the existing registry's `hosts.toml` placed temporarily under that literal combined path, a small-image Job using the actual CRI and explicit credentials succeeded. The temporary directory was removed; it is not a long-term solution.
- With user approval, around 18:05, RLark's `spec.suspended=true` was used to stop `jo-e8ec7d67138a422c` and `s1u-sync-overlap-20261105-1700` gracefully. Both reached `Stopped`, their development-host and robot Pods disappeared, and the development GPU compute process was released; the Jobs were not automatically resumed. Only this pilot registry's files were found in the two registry trust directories. The development host's CRI registry `config_path` in `/etc/containerd/config.toml` was changed to `/etc/containerd/certs.d`, and only development-host containerd was restarted. The original configuration is backed up as `/etc/containerd/config.toml.rlark-registry-8acb9ab8-3f29-4aef-98a7-3c6d33abfbf6.bak`. Before rollback, check the current config, other registry dependencies and workload impact; coordinate restore and restart in a maintenance window rather than overwriting later changes automatically. After restart, a small-image Job with explicit `imagePullSecrets` and `imagePullPolicy: Always` succeeded on `cailiang`; image layers may have been cached. Local API `/readyz`, all three local nodes and the robot node were healthy; Docker, kubelet, registry and control-plane Pods remained running. This does not establish host-reboot recovery or trust on other nodes; `alexfu` and `wendychen` still need separate setup and verification.
- To avoid ambiguity from the development host's shared Docker/containerd content cache, a 1 MiB random file was created on the robot and pushed as the unique image `192.168.31.2:5443/rlark/transfer-validation:robot-cri-03600ca5-ccdf-47e0-891a-50d3c70cf1c6`. An explicit-credential Kubernetes Job on `cailiang` succeeded with matching file SHA-256 `0653a709a781116e7da88f10ffc9fc5aea293f9c138f2c1c4630a7e867c56516`; registry logs recorded a GET of its new compressed layer. The manifest digest was `sha256:0449e2118dab467d99a342a23ca5013506672957403802f4d39100d764ef31b8`. This verifies that node's new-layer download through the registry, not a direct KS3 object check or other nodes. This image, another locally built test image `cri-364b88cb-0417-4575-b68d-1233f25b952b` and their caches remain and incur small storage costs; temporary login and build files were removed.
- The original Job CRs later disappeared from the management API, while the RLark Jobs page continued to show both Jobs as stopped and offered a restart action; no restart was performed.
- Bounded single-sample transfer: the development host pushed `192.168.31.2:5443/rlark/transfer-validation:large-f6af8d8b-7af0-45ff-a883-bb526811e836` to the KS3-backed registry. Its new random layer was 268,517,572 bytes (about 256.08 MiB); the push took 98.43 seconds, about 2.60 MiB/s. Robot Docker pulled that new layer for the first time, reusing the cached base layer; the entire pull including unpack took 80.38 seconds, about 3.19 MiB/s based on the new layer. The random file SHA-256 matched on both hosts: `96106e5e52baeeb6bf0704eb5bd76a3ac7ae9c4c0edf97f2116bf27e31a19efe`; manifest digest: `sha256:0e0e56d7812d576caff0c6e5de17e54cdee620f069edcca7b2d78b1dd8757dcb`. The image, KS3 objects and both hosts' image caches remain and incur storage costs; temporary login and original build files were removed. The large KS3 blob was not independently GET-verified: the test verified the configured backend and registry-mediated pull. One sample does not establish 15.5 GB Fleet performance, multi-layer concurrency, interruption recovery or long-term reliability.

The pilot validation image was `192.168.31.2:5443/rlark/validation:deploy-b2c57f5c-a807-47f8-868c-d2a740656302`. The registry reported manifest digest `sha256:f97baa533a26513c453a362be331a43eb60214302f449c16571b23cea141dce4`; direct KS3 GET and SHA-256 verification matched both that manifest and its roughly 2.1 MiB layer. The robot's authenticated Docker pull returned the same manifest digest. This is a small-image functional check, not a large-image benchmark.

For inspection, run `docker ps --filter name=rlark-ks3-registry` and `docker inspect rlark-ks3-registry --format '{{.State.Status}}'` on the development host; `curl --noproxy '*' -o /dev/null -w '%{http_code}\n' --cacert "$HOME/.config/rlark-registry/server.crt" https://192.168.31.2:5443/v2/` without authentication should return 401. Start or restart only this container with `docker start rlark-ks3-registry` or `docker restart rlark-ks3-registry`; a restart test passed, but reboot recovery and address availability at Docker startup have not been tested. Do not delete the container, credentials or KS3 prefix as a recovery shortcut.

To push from the development host, use a unique image tag and an isolated Docker configuration; the password stays out of command arguments. Replace the image names with the intended image and remove the temporary Docker configuration securely afterwards:

```bash
cfg=$(mktemp -d)
DOCKER_CONFIG="$cfg" docker login 192.168.31.2:5443 -u rlark --password-stdin < "$HOME/.config/rlark-registry/client-password"
docker tag IMAGE:TAG 192.168.31.2:5443/PROJECT/IMAGE:UNIQUE_TAG
DOCKER_CONFIG="$cfg" docker push 192.168.31.2:5443/PROJECT/IMAGE:UNIQUE_TAG
unlink "$cfg/config.json"
rmdir "$cfg"
```

### Service image snapshots: 2026-11-05

The three `amd64` images then running RLark Server/Controller Manager, Gateway and the development-cluster Agent were pushed to the pilot registry. Authenticated manifest reads matched the source digests. These are backups of running versions, not builds of the current working tree or a deployed upgrade; pulling them again and starting the services has not been verified. Exact tags and digests are recorded in the operations guide. The UI and the robot's current image have not been uploaded. A still cannot reach the registry directly, and certificate trust remains pending on `alexfu` and `wendychen`.

## Registry and KS3 performance test: 2026-11-05

With approval, the earlier test used unique objects under `ks3://stardust/ks3-regular/post_train/rlark/registry-test/`. The isolated run ID was `1a330a71-b31e-4f14-9a0f-9b31edde5dce`; registry data is under `registry/<run-id>/`, and benchmark objects are under `benchmark/<run-id>/`. The test objects remain in KS3 and may incur storage charges. This is separate from the later pilot prefix.

The temporary registry used Distribution 3.1.2, a protected configuration, the KS3 S3 API with signature v4 and virtual-host addressing, and downloads proxied by the registry rather than redirected to KS3. It listened only on local loopback without TLS or authentication and is **not a production deployment**. After tests, only this temporary container and its anonymous local volume were removed, the temporary credential-bearing configuration was unlinked, and the existing registry was left running unchanged. The test image tags remain in the local and robot Docker caches.

Local Docker pushed a small BusyBox image and a separate image with a 16 MiB random layer. The test registry was restarted; its manifest and layer digests matched, and a separate S3 API listing and GET confirmed the large blob existed in the intended KS3 prefix and matched SHA-256. The robot Docker daemon pulled the 16 MiB image through a temporary SSH reverse tunnel; the layer was downloaded through that tunnel and its SHA-256 matched. The tunnel has been closed. This demonstrates a tunneled robot pull, **not** direct robot-to-registry reachability, a cloud pull, Kubernetes integration, authenticated access or production availability. A separate direct S3 multipart API upload with three parts and a 16 MiB object completed and matched SHA-256; it does not by itself prove every registry multipart/retry path.

Compatibility finding: the tested Distribution 2.8.3 custom S3 endpoint forced path-style bucket addressing, which KS3 rejected with `SecondLevelDomainForbidden`. Distribution 3.1.2 used virtual-host addressing successfully in this test. Version 3's default configuration path differs from version 2's: an initial v3 run accidentally used its default local filesystem backend. Those initial push/pull results were discarded; the results above are from the corrected config at `/etc/distribution/config.yml` and independently verified KS3 objects. One S3 listing returned HTTP 429; a later listing succeeded, so its cause and sustained behavior remain unverified.

### Observed single-stream throughput

Each measurement used one 64 MiB random-content object, one sample, and the indicated host; the S3 API GET and KS3FS reads included SHA-256 verification. Writes include completion of the S3 PUT or KS3FS `fsync` and close. First local KS3FS access and robot access used newly created objects, but caching and unrelated traffic cannot be completely ruled out. The local KS3FS write was the initial run while a registry image download may have overlapped; the direct API figures are a later repeat without that test download. These are point-in-time results, not capacity, concurrency, or SLA guarantees.

| Host and method | Write | Read |
| --- | --- | --- |
| Development host, direct S3 API | 3.11 MiB/s | 3.34 MiB/s |
| Development host, KS3FS | 2.17 MiB/s | 3.21 MiB/s |
| Robot, KS3FS via scoped proxy | 2.93 MiB/s | 2.81 MiB/s |

The direct API verified both hosts' KS3FS-written objects by SHA-256. No cache-drop, concurrency, large-model, repeatability or cloud-side benchmark was performed. Re-test on the intended production path with a representative dataset and record costs before sizing large image delivery.

## Mount prerequisites and safety

- These are existing **machine-local** scripts and installations, not scripts packaged by the repository. The development host has KS3FS and FUSE 2 libraries under `~/.local/opt/rlark-ks3fs/1.3.1/`; the robot has the Ubuntu 20.04 KS3FS 1.3.1 package extracted under the same base directory and uses its system FUSE 2 library. They are not interchangeable without checking OS and library compatibility.
- Each mounting user needs `/dev/fuse`, suitable FUSE permissions, network access, and an owner-only `~/.config/rlark-ks3/passwd` owned by that user with mode `600`. The robot was authorized to use a protected copy of the development credential. Formatting it for a bucket does **not** reduce its cloud permissions; restrict policy at the provider, and update or revoke both copies on rotation. Never print the file or place its contents in command arguments.
- The scripts refuse an existing mount or nonempty underlying directory. They check mount type and dataset access, but do not prove prefix identity; if a post-mount check fails, the mount remains for investigation. Do not mount over existing local files.
- **Neither mount automatically starts or recovers after reboot.** If the mount is missing, its underlying local directory can still be writable and applications may silently write locally. Check the mount and intended data before each use; do not use for unattended jobs until fail-closed protection and recovery are tested. Other users, containers and Kubernetes workloads have not been verified.

## Development-host operations

Run as `cailiang`, not root. Check the mount and access first:

```bash
findmnt -M /mnt/ks3/post_train -o TARGET,SOURCE,FSTYPE,OPTIONS
timeout 15 test -d /mnt/ks3/post_train/datasets && echo 'datasets accessible'
```

After boot, or after a deliberate unmount, ensure the mountpoint is unmounted and empty, then run:

```bash
~/.local/bin/ks3-post-train-mount.sh
```

The script defaults to `/mnt/ks3/post_train`; `RLARK_KS3_MOUNTPOINT` can override it. When all consumers have stopped, unmount with `fusermount3 -u /mnt/ks3/post_train`. Do not unmount a live workload merely to test these instructions. For script-only simulated checks, the existing local `~/.local/bin/ks3-post-train-mount-test.sh` does not replace an end-to-end storage test.

## Robot operations and network

On `x86`, run as `astribot`, not root:

```bash
findmnt -M /mnt/ks3/post_train -o TARGET,SOURCE,FSTYPE,OPTIONS
systemctl --user status rlark-ks3-proxy.service
timeout 15 test -d /mnt/ks3/post_train/datasets && echo 'datasets accessible'
```

After boot, or after a deliberate unmount, ensure the target is unmounted and empty, then run:

```bash
~/.local/bin/ks3-post-train-mount-robot.sh
```

The script starts the user-level proxy before mounting. When all consumers have stopped, unmount with `fusermount -u /mnt/ks3/post_train`. Do not stop the proxy while a mount depends on it. The proxy service is not enabled at boot; user lingering is disabled, so the user service may stop when user sessions end. There is no automatic remount or health monitoring.

The robot's default DNS and source route did not reliably reach Ningxia KS3. The scoped proxy listens at `127.0.0.1:23793`, accepts CONNECT only to `ks3-cn-ningxia.ksyuncs.com:443` and `stardust.ks3-cn-ningxia.ksyuncs.com:443`, asks router `192.168.31.1` for DNS using source `192.168.31.250`, and binds that source for outgoing connections. KS3FS validates TLS end-to-end. Other local users can connect to the loopback proxy; the host/port allowlist does not authenticate local users. No global DNS or routing was changed. Changes to the addresses or routes can break this environment-specific workaround; do not hard-code resolved KS3 IPs.

## Troubleshooting

1. If `findmnt` shows no mount, stop consumers before examining the underlying directory. Do not assume files there reached KS3. Preserve unexpected local files; the scripts will refuse to hide them.
2. If a mount exists but `datasets/` fails or times out, check connectivity, credential validity and permissions. On the robot also inspect the proxy's user service and confirm its configured router/source addresses are still applicable. Anonymous HTTP 403 can prove an HTTPS route works, but not that credentials or writes work.
3. If a script says the mount already exists, inspect it rather than running another mount. If it says the directory is nonempty or credential permissions are unsafe, fix the cause without deleting or exposing data. Never print credentials when collecting diagnostics.
4. If the mount is stale or a check fails after mounting, first stop consumers and investigate before unmounting or retrying. Neither script guarantees automatic recovery; don't assume a successful remount validates container access or the entire prefix.

## Execution and acceptance gates

1. **Access and recovery:** confirm approved prefixes, policy, quota and cost ownership. Validate scoped credentials, cache bounds, rename/locking limits, network interruption, remount and fail-closed handling on each host. Design boot lifecycle and alerting before unattended use.
2. **Cloud:** inspect existing A/B mounts without changing workloads, then validate the selected cloud mapping and bidirectional visibility with isolated objects. Do not assume B has available resources.
3. **Code and data:** preserve existing worktrees and uncommitted changes. Test editor, Git, builds and cross-host access on an isolated tree; agree on migration and rollback before moving RLark or Fleet. Keep one writer until semantics are proven; use versioned artifacts if they are insufficient.
4. **Registry:** complete least-privilege credentials, account separation, TLS renewal, local disk limits, monitoring and backup/restore. Validate reboot, interruptions, large images, and each required client's network path and runtime trust before production use.
5. **RLark and workloads:** verify deployed Agent automatic credential matching and an actual RLark Job; provision trust on the other intended nodes. Validate cloud pulls and container access to an isolated run directory. Do not assume the incomplete Raw runtime can run OCI images.
6. **Database and end-to-end:** design PostgreSQL backup and tested restore separately; verify selected code revision, dataset, run output and image together, including storage/network failure and restart recovery. Record results and remaining limits.

Confirm targets and effects before changing robot networking, restarting workloads, creating billable services or migrating data. Keep secrets out of documentation and version control.

## Kingsoft Cloud KS3 mount validation: 2026-11-12

The optional `mount_endpoint` is implemented: Gateway retains the public endpoint and CSI uses the internal hostname, without DNS changes or fixed IPs. The independent declaration `ks3-post-train-ksyun` targets only `rlark-ksyun-gpu`; its StorageClass is `sc-1ff57f5f06154dfd`. The original declaration still targets only local and robot clusters.

Two short-lived RLark Jobs on `10-0-2-196` used Pod-level ephemeral PVCs and read-only `/mnt/ks3` mounts to read an existing 92-byte validation object. Its SHA-256 matched the public API result; no KS3 data was written. Initial reads failed with I/O errors; CSI logs showed KS3 ListObjects HTTP 429 with an HTML response, followed by recovery on retry. The second new Job also failed before recovering. Bound PVCs and Running Pods do not establish successful reads: check read exit codes and content hashes, and use bounded application retries. The cause of 429 remains unresolved; large files, concurrency, other nodes, long-term reliability and writes are not validated.

The live Gateway uses `rlark-gateway:ks3-mount-endpoint-20261112`; the stopped previous container `rlark-gateway-pre-ks3-20261112` is retained for rollback. A protected configuration snapshot is at `~/.local/share/rlark-runtime/gateway-before-ks3-20261112.json` on the development host. Authentication and field round-trip were checked; the UI development service loads the new form from this worktree. Stop Jobs using the new declaration before reverting to a Gateway that lacks this field; older editors cannot preserve the override and must not edit this declaration. Both test Jobs, Tasks, Pods, PVCs and their Retain PVs were confirmed removed. The first test mount reported busy during unmount; after confirming its test container had terminated, only that test mount was unmounted for cleanup. Shared CSI was not restarted and existing NAS workloads remained running. Credentials remain in protected management and delivered Secrets and are not recorded here.

To use it, select the Kingsoft Cloud cluster and `ks3-post-train-ksyun` in a Job and choose the application mount path. Validation covers only `10-0-2-196`; do not schedule this volume onto nodes without CSI.

## Fleet KS3 implementation snapshot: 2026-11-12

Created through the upgraded Gateway: `fleet-ks3-ksyun` → `sc-801ae48b73084190`, targeting only `rlark-ksyun-gpu`, with `prefix=ks3-regular`, `remotePath=stardust/ks3-regular`, and `vfs-cache-mode=off`. It inherits the existing public Gateway/internal CSI endpoint pair. The previous `ks3-post-train-ksyun` declaration was compared and remained unchanged. Gateway rollback container: `rlark-gateway-pre-fleet-storage-20261112`.

Acceptance is **pending**. On `10-0-2-196`, a 491,520-byte streaming write twice returned EIO from close with rclone `corrupted on transfer`. Independent signed KS3 GET returned exactly the original content and SHA-256, while its ETag was different from the content MD5. This is evidence of an ETag verification incompatibility in this tested path, not proof of general upload correctness. Do not disable integrity checks or switch Fleet Runs on this evidence. A five-byte object also reached KS3, but large checkpoints and interrupted-upload recovery are not validated. Isolated validation objects and resources are cleaned separately; existing data is untouched.

The active Run's `/root/fleet/code/fleet/runners/common/archive.py` currently accepts only `fuse.ks3fs` in `Archiver.mounted()`. Its actual deployed version therefore does not yet demonstrate the reported rclone cache-mode probe. Preserve its startup check and validate the intended Fleet version before switching. No Fleet code or Run configuration was modified.


## Resumed execution and acceptance: 2026-11-12 09:29 UTC

Execution resumed after the user confirmed the other operation had finished. Cluster Agent was built on the just-migrated registry version with only its Agent binary replaced: `10.254.240.1:5443/rlark/cloud-agent@sha256:7515240c5d9f24640f7a6f92f052f943a12be3eb8dfe794e49c1dc6852162c3f`. Existing arguments, environment and workload `--image` were retained. Addon generation 5 first applied `nodeUpdateStrategy=OnDelete` and `nodeImagePullPolicy=IfNotPresent`. After checking the live DaemonSet strategy, generation 6 cleared `nodeSelector` and changed all four CSI images to verified digests at `10.254.240.1:5443/csi-rclone/*@sha256:...`. Controller placement remains `10-0-2-196`.

Both node plugins are 3/3 Ready and both `CSINode` objects register `rclone.csi.veloxpack.io`. The original node-196 Pod `csi-driver-rclone-node-cwgw2` retains UID `f896bb7a-9c28-49a6-ba9b-6709eee7937a` with zero restarts in all three containers. Node 57 received `csi-driver-rclone-node-4pwh7`. With `OnDelete`, node 196 retains its old template and the same driver image; the new image address takes effect only after an explicit maintenance replacement. Do not delete an active mounting Pod just to make the updated replica count equal the desired count.

Isolated PVCs/Pods mounted `s3:stardust/ks3-regular` as `fuse.rclone` on both nodes; CSI requests confirmed `vfs-cache-mode=off`. Each node passed a five-byte write, close, rename and local SHA-256 readback. Node 57 also read the existing 491,520-byte validation object written by node 196 through its mount, matching SHA-256 `2c2cd22ad2454be571aafde3be545ec44db86330585a860d1af8e274a493e203`. This proves that existing object was readable across nodes; it does not justify ignoring a failed close. A 491,520-byte write failed with close EIO on both nodes. **The new class has not passed Fleet write acceptance; do not switch Runs.**

An independent S3 singlepart/multipart comparison uploaded identical content and downloaded it to verify SHA-256. Singlepart and multipart-part ETags equal content MD5 `0302637dadb24a465621d1be04717fb3`; the completed multipart object's ETag is `b2931c9fb143d6a49b330182336d1a52`, matching the rclone failure. CSI v0.2.0 embeds rclone v1.71.2, whose S3 backend interprets a 32-hex ETag as MD5. The tested KS3 multipart path violates this assumption. Fix or upgrade KS3 multipart verification while preserving actual content verification. Merely disabling `use_multipart_etag` or setting `disable_checksum` does not fix the final streaming-transfer comparison; bypassing integrity verification is not a repair.

Two more acceptance limits remain: the default 5 MiB multipart chunk size supports only about 48.8 GiB for unknown-size streams, below an 80 GB checkpoint, so a suitable chunk size must be configured and tested with the compatibility fix. Immediate reads of new files from the other node returned ENOENT before its directory cache refreshed; immediate cross-node visibility is not established. Large-file validation, close errors during network interruption, and full-file retry remain unaccepted. No shared CSI process, node network or real Run was fault-injected. After the fix, rerun these tests, verify the intended Fleet version's rclone cache probe, and switch only after the current Run ends.

This session did not modify Fleet or DataTools Jobs. During validation, management Tasks were deleted/recreated and the controllers removed the old Fleet and two DataTools Pods, so workload UID continuity cannot be claimed. CSI Pod identity was checked independently. Both NAS directories in the recreated DataTools Pod remained readable by stat and mounted as `fuse.rclone`. Cleanup is limited to the unique test prefix and test Pods/PVCs/Retain PVs with verified Pod/claim UIDs; business data is retained.

Recovery: the previous Agent was `10.254.240.1:5443/rlark/cloud-agent@sha256:3c8e38c898654ff120ceaf0216cd661d987f0049e213a428ec54a13131d35be1`. Before reverting it, restore the Addon's former image values and node selector: the old catalog ignores the new controls and may restore RollingUpdate. That restoration removes the node-57 plugin and requires proving no active CSI volumes use it. Do not directly revert the Agent or enable RollingUpdate with active mounts. Prefer retaining the working plugins/current Addon and continuing KS3 fixes in isolated tests.
