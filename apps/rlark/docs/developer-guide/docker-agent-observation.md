# Docker host Agent observation (preview)

This increment adds explicit, read-only Docker host registration and heartbeat
observation to the independent `extensions/container-runtime` module. It does
not dispatch Docker Jobs. Raw execution and its existing packages remain the
default. The Kingsoft Cloud notebook has no Docker socket and therefore only
validates Raw; it cannot validate this Docker Engine path.

## Implementation plan and acceptance boundary

1. Register a dedicated Docker Cluster and exact native/companion Node through
   the administrator CLI. Set `instanceID` to the host Engine's `docker info
   --format '{{.ID}}'` result. Keep tenant, private network scope, environment,
   and an explicit authorized GPU UUID allowlist. GPU visibility is not entitlement.
2. Export a private, observation-only Agent package. It contains an mTLS
   certificate, Agent, checksum launcher and root systemd unit; it contains no
   Raw supervisor, workload paths, issuer private key or management kubeconfig.
3. Run `--runtime=docker` against a local host Unix socket. The Agent reads Engine
   info/version, managed-container occupancy, and local NVIDIA UUID visibility.
   Its ten-second observation deadline bounds daemon/GPU outages. It never
   starts a daemon, creates a container or mounts a socket into a workload.
4. Bind the observation to the registered runtime and Engine ID server-side.
   Replacement, credential mismatch, incomplete info, or failed inventory cannot
   produce a fresh Ready heartbeat. Raw legacy heartbeats remain readable.
5. Test failed/recovered daemon calls, canceled contexts, retained containers,
   Engine replacement, wrong runtime, certificate revocation and package export.
   Test the official Docker SDK over a local Unix socket as well as a fake
   Engine. Build the module and both documentation languages before review.

`Ready=True` means a current authenticated observation, while `Schedulable=False`
is retained for every Docker observation in this increment. No workload or
upgrade capability is advertised. Container creation/start/stop/removal, scoped
logs/stats, Docker Job scheduling, GPU device assignment, Ray readiness/port
barriers and Docker Agent upgrades require subsequent implementation and
separate Docker host acceptance. Existing provider unit tests are not cloud
Docker Engine acceptance.

## Operator workflow

Build with `make -C extensions/container-runtime build`. Use the existing
`container-runtime-admin cluster-create`, `node-register` and `node-package`
commands with `--kubeconfig` and strict `--config` JSON inputs. Start from
`config/examples/provision-cluster.json` and `provision-node.json` in the
extension directory. Set the cluster `runtime` to `Docker`; use new operation
UUIDs and the returned exact Cluster UID. Run on a dedicated Linux Docker host
as root, with a working NVIDIA runtime and operator-verified GPU entitlement.
Do not run Docker-in-Docker or modify the cloud notebook to pretend it is a host.

Use `extensions/container-runtime/config/examples/provision-docker-package.json`
as the Docker package template. Set `runtime` explicitly to `Docker` (omitted
runtime defaults to `Raw`), retain the common certificate/artifact/identity fields,
and set `dockerHost` to
`unix:///var/run/docker.sock` (the default), and omit `supervisorBinary`,
`stateDirectory`, `executables`, `workDirectories`, and `logBytes`. Do not enable
execution, logs or upgrades; unsupported options are rejected before any package
directory is created. Use a new private output/install directory. Export stays
on the administrator machine, copy its entire directory securely to the exact
host install path, and protect it from workload writes. The signer and kubeconfig
never leave the administrator machine. Automatic enrollment/renewal is incomplete.

The generated `start-agent` runs in the foreground. If systemd is available,
install its `rlark-container-agent.service` under `/etc/systemd/system/`, then
run `systemctl daemon-reload` and `systemctl enable --now
rlark-container-agent.service`. Its unit orders after `docker.service`; it does
not install or start Docker. A custom local socket is supported only as a
canonical absolute `unix:///...` path. Remote TCP/TLS daemon transport is not
part of this Agent contract.

The manual equivalent, using the exported credentials, is:

```bash
/opt/rlark-docker/container-runtime-agent \
  --runtime=docker \
  --docker-host=unix:///var/run/docker.sock \
  --endpoint=https://container-api.example.internal:9444 \
  --tls-cert=/opt/rlark-docker/agent.crt \
  --tls-key=/opt/rlark-docker/agent.key \
  --server-ca=/opt/rlark-docker/server-ca.crt \
  --version=preview-v1 \
  --private-address=10.0.3.85
```

No supervisor socket is needed in Docker mode. Raw remains `--runtime=raw`
(default) and requires `--supervisor-socket`. Mixing runtime-specific flags or
enabling unsupported Docker operations fails configuration validation.

## Observation and recovery semantics

Capacity is the Docker daemon's total CPU/memory and the administrator-authorized
GPU allowlist, not free resources or per-task utilization. Docker Engine API
1.42 or newer within major version 1 is required. NVIDIA runtime presence is a
configuration check; it does not prove a GPU container can launch. All authorized UUIDs
must be visible and a configured NVIDIA runtime must be present. Unknown or
incomplete runtime/version information fails observation. Raw heartbeat fields
may be omitted by older Agents; Docker observations require `runtime: Docker`
and the exact `instanceID`. Extra visible devices do not enlarge entitlement.

Any managed container retained on the daemon, including exited/dead containers,
keeps `Busy=True` until removed by its proven owner. This inventory is a
conservative local observation, not an allocation lock or permission to adopt
or delete it. Foreign, unlabeled containers are not modified; operators must
dedicate the host because this observation does not reserve their ports or GPUs.

A failed Docker info/version/list or NVIDIA query sends no heartbeat. The Agent
retries with bounded exponential backoff, re-reading current state rather than
replaying a stale successful observation. The controller expires readiness after
45 seconds, with status repair every 30 seconds. Recovery of the same daemon resumes observations. A different
Engine ID is rejected; cordon and retire the exact old Node through normal
cleanup, register a new instance and issue new credentials. Do not edit the old
binding, reuse its credentials or force-delete uncertain allocations.
