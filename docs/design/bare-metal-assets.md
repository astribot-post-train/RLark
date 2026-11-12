# Kingsoft Cloud Bare Metal Assets

Last verified: 2026-11-12. This page records team access routing and observed node roles. It contains no login password, private key, or object-storage credential.

| SSH entry (user `root`) | Kubernetes node | Observed role | Kubernetes state |
| --- | --- | --- | --- |
| `36.103.180.169:2222` | `10-0-2-57` | Control-plane node | Ready, Kubernetes 1.31.14 |
| `36.103.180.169:2223` | `10-0-2-196` | Worker / GPU node | Ready, Kubernetes 1.31.14 |

The host and port mapping was verified by SSH on 2026-11-12. The cluster reports node addresses `10.0.2.57` and `10.0.2.196`, respectively. Credentials are not stored in this repository; use the team's approved credential manager.

## rclone CSI coverage

On 2026-11-12, `rlark-system/csi-driver-rclone-node` selected only `kubernetes.io/hostname=10-0-2-196`; its DaemonSet had one desired and ready Pod. It already had the broad `operator: Exists` toleration, so the control-plane taint was not the exclusion cause. The hostname selector excluded `10-0-2-57`. The RLark Addon catalog supports an optional node selector; empty means no hostname restriction. Change the cluster's RLark-managed Addon and verify both node Pods and CSI registrations after reconciliation. Do not manage the DaemonSet independently of its Addon owner.

## KS3 for Fleet

Gateway uses `https://ks3-cn-ningxia.ksyuncs.com`. The existing cloud class uses `https://ks3-cn-ningxia-internal.ksyuncs.com` for CSI mounts. A new unauthenticated TLS probe from `10-0-2-196` to the public bucket hostname timed out after 5 seconds; a 403 observed through a different route is not proof that this direct route works. The new class inherits the previously validated endpoint pair. Gateway creation must pass authenticated `HeadBucket`. The Fleet-specific class is intended to use bucket `stardust`, object prefix `ks3-regular`, virtual-host addressing, and `vfs-cache-mode=off`; keep the existing class unchanged. `remotePath` should therefore resolve to `stardust/ks3-regular`, matching the existing `ks3://stardust/ks3-regular/` tree.

Keep Fleet's startup check. Before switching a Run, validate a new isolated Pod on each node, confirm cache mode and mounted prefix, and test close-error propagation plus full-file retry after an interrupted upload. Do not switch an active Run.

See the [Chinese asset page](bare-metal-assets.zh-CN.md) and [Station deployment notes](station-deploy/README.md).

## Fleet KS3 implementation snapshot: 2026-11-12

The existing declarations remain required while Fleet archive acceptance is pending:

| Declaration | Scope and purpose | Status |
| --- | --- | --- |
| `ks3-post-train` | Local and robot clusters; existing shared reads | Keep |
| `ks3-post-train-ksyun` | Cloud cluster; read-only model/data access and validation | Keep |
| `fleet-ks3-ksyun` | Cloud cluster; `ks3-regular` archive candidate | Keep for isolated tests; do not use for active Runs |

Do not remove either `ks3-post-train` declaration until live PVC and Job references have been checked. The cloud class cannot be retired merely because the Fleet-specific class exists: the latter still fails the KS3 multipart close test.

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
