# Private Image Registry Operations (Pilot)

On 2026-11-12, robot worker `s1u-02-x86` reached `10.11.14.128:5443` through a persistent host route sourced from `192.168.31.251`. Its dedicated containerd was configured with the existing registry CA and completed a TLS-verified authenticated `busybox:latest` pull. This validates that worker and image at that time; it does not validate automatic credential distribution, other repositories, or cross-node Pod networking.


Updated 2026-11-12 (topology section); other sections last updated 2026-11-05. This environment-specific guide is not a production-readiness claim. See the [shared storage and registry plan](../design/shared-storage-registry-plan.md) for design, earlier tests and open work. [中文](private-registry.zh-CN.md)

## Current registry topology: 2026-11-12

This section supersedes the address table below where they differ. The `192.168.31.2:5443` address and the `rlark-ks3-registry` container described later are historical.

| Entry | Served by | Storage | Who uses it |
| --- | --- | --- | --- |
| `10.11.14.128:5443` (HTTPS, `rlark` account) | `rlark-ks3-registry-migrated` container on the development host | KS3 `stardust/ks3-regular/post_train/rlark/registry` | Development host pushes; RLark credential `ir-e9a22bbb4e156a1b` (Local registry 10.11.14.128) |
| `10.254.240.1:5443` (WireGuard, plain HTTP, no auth) | `rlark-ks3-registry-wg` container on the development host | Same KS3 path | Image name used by Kingsoft cloud workloads; cloud nodes resolve it through their local mirror first |
| `127.0.0.1:5444` on each cloud node (read-only) | `rlark-registry/rlark-registry-mirror` DaemonSet | Same KS3 path, KS3 internal endpoint | containerd `certs.d` mirror for `10.11.14.128:5443` and `10.254.240.1:5443`; falls back to `10.254.240.1:5443` over WireGuard |
| `10.254.240.2:5443` (HTTPS, `rlark` account, writable) | `rlark-registry/rlark-registry-cloud` Deployment on `10-0-2-57`, `hostNetwork`, bound to the WireGuard address only | Same KS3 path, KS3 internal endpoint | Pushes from the Kingsoft cloud; RLark credential `ir-667c8af90d294c2b` (金山云 KS3, store only) |
| `localhost:5555`, `10.11.14.128:5555`, `10.254.240.1:5555` | `local-registry` container on the development host | Local Docker volume, **not** KS3 | Development cluster and robot cluster |

**Placement rule.** The development cluster and the robot cluster pull from `local-registry` (`:5555`). The Kingsoft cloud cluster pulls from KS3: reference `10.254.240.1:5443/<repo>@sha256:<digest>` so that nodes read through their local KS3 mirror. An image used by cloud workloads must exist in KS3 before it is referenced; push it to `10.254.240.2:5443` from the cloud, to `10.11.14.128:5443` from the development host, or copy it from `:5555` with `skopeo copy --all --preserve-digests`. All KS3 entries share one storage path, so an image pushed through any of them is visible through all of them.

**Naming.** Use namespaced repositories: `rlark/agent`, `rlark/cloud-agent`, `rlark/gateway`, `rlark/controller-manager`, `rlark/station-*`, `data-tools/*`, `fleet/*`. Push a time-prefixed tag `YYYYMMDD-HHMM-<alias>` together with the plain `<alias>`, and pin deployments by manifest digest.

**Migration record (2026-11-12).** All 35 tags in `local-registry` and 19 images that existed only in the development host Docker cache were copied to KS3 under the names above, with digests verified after copy; one existing KS3 tag (`rlark/station-agent:wg-observation-20261111`, pointing to the running `c5b1b05f…`) was kept rather than overwritten. Each copy also received a `20261112-1541-<tag>` alias. Three images pushed to `:5555` after the bulk copy (`rlark-cloud-agent:fleet-storage-20261112`, `station-controller:enroll-20261112`, `station-services:enroll-20261112`) were copied separately. The cloud `rlark-agent` (`rlark/cloud-agent@sha256:3c8e38c8…`, `--image=rlark/agent@sha256:82c2d7b8…`), `rlark-agent-node` and two data_tools Jobs now reference `10.254.240.1:5443`. The development and robot clusters were intentionally not changed, and `local-registry` keeps running. `kube-system/rlark-nvidia-device-plugin` on the cloud uses each cloud node's own `localhost:5555` and is unrelated. To roll back a cloud workload, set its image back to the `10.254.240.1:5555/...` reference; the previous manifests were backed up outside Git.

**Cloud registry service.** The deployment is described in [ksyun-registry.yaml](ksyun-registry.yaml); Secrets are created separately. The TLS certificate (`CN=rlark-registry-ksyun`, `IP:10.254.240.2`, valid until 2027-11-12) is signed by the same `rlark-registry-local` CA as the development-host registry, so the Gateway's existing `SSL_CERT_FILE` trust and `--registry-browse-allowed-hosts` CIDR already cover it. It reuses the `rlark` htpasswd, reads KS3 keys from Secret `rlark-registry-mirror-s3`, disables backend redirects and **disables deletion**, because every entry shares the same storage. Verified: unauthenticated `401`, authenticated catalog and manifest reads, an upload session that was opened and cancelled, and Gateway browsing with build times. Not verified: a full image push from a cloud host, node reboot recovery, and use of `10.254.240.2:5443` directly as a Pod image reference (nodes do not yet trust its CA or hold a pull credential for it; use the `10.254.240.1:5443` name instead). Do not run garbage collection while any registry entry is writable.

## Registry and access

| Item | Current value |
| --- | --- |
| Registry address | `192.168.31.2:5443`, bound only to this development-host LAN address |
| Backend | `ks3://stardust/ks3-regular/post_train/rlark/registry/`, accessed by the registry using the S3 API |
| Username | `rlark` |
| Password | `~/.config/rlark-registry/client-password` for `cailiang` on the development host; mode `600` |
| TLS | Self-signed certificate `~/.config/rlark-registry/server.crt`, expires 2027-11-05; clients need dedicated trust |
| RLark credential | `RLark KS3 Registry (pilot)`, selected for the development and robot clusters, delivered to `rlark-system` |

To view the password when necessary, run `cat ~/.config/rlark-registry/client-password` in a private terminal on the development host. Do not send it to chat, include it in screenshots or Git, or pass it as a command argument. The development-host login below reads the file without displaying the password.

**The single pilot account permits both push and pull.** Anyone who can read the distributed Kubernetes Secrets can also push. There is no read-only account, repository-level authorization or complete audit. Do not provide the credential to untrusted users or hosts. Clients need only the registry credential and public certificate, not the KS3 credential or server private key. Image downloads are proxied by the development host, so host, network or KS3 outages affect pulls.

## Which images can be uploaded

RLark services and UI, Fleet/rollout, and robot runtime Docker/OCI images can use separate repository names. Uploading an image does not prove it runs on every target. Verify provenance, licensing, architecture, drivers and dependencies, and check that the image contains no secrets, training data or model checkpoints that should not be published. Current development-cluster nodes and robot `x86` report `amd64`; build or validate a multi-architecture image separately for other architectures. Do not assume a local single-platform Docker tag/push preserves an original multi-platform index.

Suggested names use a new version, Git revision and architecture for each release; do not overwrite tags in use:

```text
192.168.31.2:5443/rlark/control-plane:<version>-amd64
192.168.31.2:5443/rlark/ui:<version>-amd64
192.168.31.2:5443/fleet/rollout:<version>-amd64
192.168.31.2:5443/robot/runtime:<version>-amd64
```

These are naming examples, not evidence that the images have been uploaded. Check free space, network and KS3 charges before pushing. The development host's Docker listing shows an existing Fleet GPU image at approximately 15.5 GB; actual transfer depends on compression and layer reuse. One approximately 256 MiB random layer was pushed and pulled by the robot, but this does not establish duration, reliability or recovery for a 15.5 GB image; see the [design and progress record](../design/shared-storage-registry-plan.md). A KS3 backend does not eliminate local disk use by builds, Docker/containerd caches or unpacking.

## Uploaded service snapshots: 2026-11-05

These are backups of the versions running at the time, not builds of the current working tree or a completed upgrade. Authenticated registry manifest reads matched the recorded digests; pulling the images again and starting the services has not been verified. Pin the manifest digest for use and do not overwrite these tags.

| Purpose | Registry image | Manifest digest |
| --- | --- | --- |
| Server, Controller Manager | `192.168.31.2:5443/rlark/control-plane:snapshot-20261105-0a32522f-f2fa86cd9016-amd64` | `sha256:f2fa86cd9016788b1dccb48fc9315585abecbed4390a8bfa3046e5d55513804e` |
| Gateway | `192.168.31.2:5443/rlark/gateway:snapshot-20261105-0a32522f-7a9216e3e37b-amd64` | `sha256:7a9216e3e37b39d64a141cc08e5e69cdda715bea38e154abc041fe6fed3cd570` |
| Development cluster Agent | `192.168.31.2:5443/rlark/agent:snapshot-20261105-0a32522f-cb1fcfb7c0a4-amd64` | `sha256:cb1fcfb7c0a467db0a4490aa92b79754829788aa280a369c1b291a071cf65624` |

These snapshots do not automatically update running workloads. The UI and the robot's separate current image have not been uploaded. Cloud A cannot directly reach this registry, and container runtime trust is not yet configured on `alexfu` or `wendychen`.

## Development host: login, push and pull

Run on the development host with Docker and the registry-specific CA installed. First inspect available images and the architecture of the intended source:

```bash
docker image ls
docker image inspect fleet-runtime-gpu:dev --format 'os={{.Os}} arch={{.Architecture}}'
```

`fleet-runtime-gpu:dev` is an observed local image, not a guarantee that it is the intended rollout release. The source must exist in the Docker daemon of the **machine running the commands**. Identical names on the development host and robot do not prove identical contents.

The following complete example uses an isolated temporary Docker credential directory. Replace `SOURCE_IMAGE:TAG` and `UNIQUE_TAG`, and run in one shell. Record the manifest digest printed by Docker and the source version for deployment and rollback. Do not proceed with publishing after an error.

```bash
(
  set -eu
  cfg=$(mktemp -d)
  trap 'rm -f -- "$cfg/config.json"; rmdir -- "$cfg"' EXIT
  export DOCKER_CONFIG="$cfg"
  docker login 192.168.31.2:5443 -u rlark --password-stdin \
    < "$HOME/.config/rlark-registry/client-password"
  docker tag SOURCE_IMAGE:TAG 192.168.31.2:5443/fleet/rollout:UNIQUE_TAG
  docker push 192.168.31.2:5443/fleet/rollout:UNIQUE_TAG
  docker pull 192.168.31.2:5443/fleet/rollout:UNIQUE_TAG
  docker image inspect 192.168.31.2:5443/fleet/rollout:UNIQUE_TAG \
    --format 'os={{.Os}} arch={{.Architecture}} digests={{json .RepoDigests}}'
)
```

Docker stores login data in the temporary configuration. The example cleans it up on normal exit; after a forced termination, inspect and securely remove any remaining temporary directory. To test a previously verified small image, substitute `192.168.31.2:5443/rlark/validation:deploy-b2c57f5c-a807-47f8-868c-d2a740656302` as the pull address. It is not a rollout image.

RLark source also supplies local image build targets, for example `make docker-build-rlark` and `make docker-build-rlark-ui` from the repository root; publish the resulting image as above. Refer to the relevant Makefile for default tags and buildx platforms. Builds require the base images and dependencies to be available and do not switch running RLark services.

## Robot and other clients

Robot `x86` Docker and its Kubernetes-specific containerd have this registry CA installed. Its Docker daemon requires `sudo`. In a private robot terminal, log in interactively; the password prompt does not echo the input. If needed, obtain the password privately on the development host as described above. This example uses temporary configuration and the verified small image:

```bash
(
  set -eu
  cfg=$(mktemp -d)
  trap 'rm -f -- "$cfg/config.json"; rmdir -- "$cfg"' EXIT
  export DOCKER_CONFIG="$cfg"
  docker login 192.168.31.2:5443 -u rlark
  sudo env DOCKER_CONFIG="$cfg" docker pull \
    192.168.31.2:5443/rlark/validation:deploy-b2c57f5c-a807-47f8-868c-d2a740656302
)
```

If an image exists only in robot Docker, inspect it there, use `sudo docker tag`, and push with `sudo env DOCKER_CONFIG="$cfg" docker push ...` in the same authenticated temporary session. Confirm its contents and costs first. Trust in Docker does not mean every Kubernetes containerd runtime trusts the registry.

For a new client, first confirm connectivity to `192.168.31.2:5443`. An administrator must securely provide the public CA, configure trust in the relevant container runtime and verify TLS. Do not use `insecure-registries` or disable certificate checks as a long-term solution. Cloud A's direct connection timed out; a TLS-verified authenticated manifest request succeeded through a temporary SSH reverse tunnel. This is not a persistent connection or a container-runtime pull; the tunnel was closed, and B has not been tested. Validate each other target node separately.

## Using RLark and Kubernetes

The deployed RLark administrator **Image Registries** page stores and distributes this credential; it does not build or push images. Set a complete image reference in a Job, for example `192.168.31.2:5443/fleet/rollout:UNIQUE_TAG`; `localhost` denotes different machines on different clients. After validation, prefer the pushed manifest digest to pin a release, for example `192.168.31.2:5443/fleet/rollout@sha256:<digest>`.

On 2026-11-05 the local `rlark-gateway` container was switched to the locally built `rlark-gateway:registry-browse-20261105` (image ID `sha256:595ea08e40c0c06e3ac5dbdb74233ac391603c8b6b37ded5b042e0f2330aba2b`). It was built from the working tree, including uncommitted changes, on the previous Gateway image; it has not been pushed to the registry and is not a reproducible release. It retains the previous database, Kubernetes and container-runtime settings, exposes the original port and uses `unless-stopped`. It allows only `192.168.31.2:5443` for browsing; the registry CA is in the image and must be replaced when the certificate changes. Do not disable TLS verification.

At `http://127.0.0.1:5173`, authenticated read-only checks returned the registry catalog, tags and a manifest digest as well as users, clusters, container-runtime clusters and Jobs. This is a local pilot, not a full regression, workload-submission or restart/reboot acceptance. The Vite UI itself is a development process, not a durable UI deployment. To roll back during an agreed maintenance window, preserve the current container under an unused name, then restore the stopped `rlark-gateway-rollback-20261105-registry` container as `rlark-gateway` and start it. The rollback returns the old Gateway without the browser endpoints; verify the service afterward. Do not remove the rollback container or old image until recovery has been validated.

A robot Kubernetes Job pulled the test image with explicit `imagePullSecrets`; the test credential name is `ir-a818414375f542f8`, and it is available only in the namespaces to which it was delivered. Current RLark source includes prefix-based credential matching, but **automatic injection by the deployed version and an end-to-end RLark Job remain unverified**; inspect the resulting Pod. Development node `cailiang` now uses a single containerd CRI trust directory; after its runtime was restarted, an explicit-credential Kubernetes Job pulled and verified a new 1 MiB random layer built on the robot. Other development nodes and cloud runtimes have not passed trust validation. See the [design and progress record](../design/shared-storage-registry-plan.md) for the configuration backup and maintenance record. Do not restart busy containerd or replace active workloads without coordination.

## Troubleshooting and recovery

```bash
docker ps --filter name=rlark-ks3-registry
docker inspect rlark-ks3-registry --format '{{.State.Status}}'
curl --noproxy '*' -sS -o /dev/null -w '%{http_code}\n' \
  --cacert "$HOME/.config/rlark-registry/server.crt" https://192.168.31.2:5443/v2/
```

- An unauthenticated `401` is expected. For failed login distinguish credentials, permissions and networking; do not include passwords in diagnostics.
- For `x509: certificate signed by unknown authority`, inspect the **actual pulling node and runtime** and its CA. Docker success does not prove Kubernetes containerd trusts it. Certificate expiry or address changes also require coordinated certificate and client updates.
- For connection failures or timeouts, check the container, development-host address, client routing and firewall. The registry binds only that LAN address; A's direct connection timed out, and a temporary tunnel does not establish durable access.
- For slow or failed transfers, inspect KS3 connectivity, capacity and costs. Do not repeatedly push large images concurrently, delete objects directly, or run garbage collection. Image layers and Docker/containerd caches can consume local disk.
- If a restart is necessary, restart only the separate service with `docker restart rlark-ks3-registry`. Container restart was tested, but host reboot recovery and address readiness remain unverified. Do not delete the container configuration, certificate, credentials or KS3 data as a recovery shortcut.

See the [design and progress record](../design/shared-storage-registry-plan.md) for further details and security limitations.
