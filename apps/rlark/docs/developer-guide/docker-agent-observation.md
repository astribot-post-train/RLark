# Docker host Agent and execution (preview)

The independent `extensions/container-runtime` module provides Docker host
observation and opt-in single-container execution. Default Docker packages still
observe only. The Kingsoft Cloud notebook has no Docker socket: its successful
Raw GPU tests do not establish Docker Engine acceptance. No Docker-in-Docker is
used, and the host socket is never mounted into workload containers.

## Prerequisites and ownership

Use a dedicated Linux Docker host with root, Docker Engine API 1.42+ within major
version 1, NVIDIA Container Toolkit/runtime, private IPv4 reachability and an
operator-authorized whole-GPU UUID allowlist. Register a separate Docker Cluster
with the independent administrator CLI. Each host is one ContainerNode, pinned
to native Cluster/Node UIDs, tenant, private address/network scope, environment,
credential and Engine ID (`docker info --format '{{.ID}}'`).

Agent capacity comes from Engine CPU/memory totals and the authorized UUID list,
not free capacity. Visible GPUs never enlarge entitlement. Host networking and
root Agent access require a trusted dedicated host. Foreign containers are never
adopted or deleted; their GPU/port usage is not reserved by RLark.

## Package and startup

Build with `make -C extensions/container-runtime build`. Use
`container-runtime-admin cluster-create`, `node-register`, and `node-package`
with administrator `--kubeconfig` and strict JSON `--config`. Start from the
extension's `config/examples/provision-cluster.json`, `provision-node.json` and
`provision-docker-package.json`. Set runtime to `Docker`, preserve operation UUIDs
on retry, and use the returned exact object UIDs.

The package contains the Agent, node mTLS credentials, public API CA, checksums,
foreground launcher and root systemd unit ordered after `docker.service`. It
contains no issuer private key, management kubeconfig or Raw supervisor. Copy
it privately to the exact install directory. Automatic enrollment/renewal and
Docker Agent upgrades remain incomplete. Canonical local `unix:///...` sockets
are supported; remote TCP/TLS Engines are not supported by this Agent.

For observation only, keep `enableExecution: false` and omit `stateDirectory`.
For execution, set `enableExecution: true` and
`stateDirectory: /var/lib/rlark-docker`, separate from the install directory.
Before first startup, provision that persistent directory as root with mode
0700. Keep `enableLogReads` and `enableUpgrades` false; omit `supervisorBinary`,
`executables`, `workDirectories` and `logBytes`. Invalid combinations fail before
package output is written.

Manual execution startup with exported credentials:

```bash
# First installation only. Preserve this directory and its records thereafter.
install -d -m 0700 -o root -g root /var/lib/rlark-docker
/opt/rlark-docker/container-runtime-agent \
  --runtime=docker \
  --docker-host=unix:///var/run/docker.sock \
  --endpoint=https://container-api.example.internal:9444 \
  --tls-cert=/opt/rlark-docker/agent.crt \
  --tls-key=/opt/rlark-docker/agent.key \
  --server-ca=/opt/rlark-docker/server-ca.crt \
  --version=preview-v1 \
  --private-address=10.0.3.85 \
  --enable-task-execution=true \
  --state-directory=/var/lib/rlark-docker
```

For systemd, install the generated `rlark-container-agent.service`, run
`systemctl daemon-reload` and enable/start the unit. It does not install or start
Docker. Raw remains a separate default mode requiring `--supervisor-socket`.

## Scheduling and lifecycle

Install the companion CRDs and controller/API RBAC. Enable controller
`--enable-job-reservations=true` and API `--enable-task-execution=true` in addition
to the Agent flag. Use `config/examples/docker-container-job.yaml` and its
ContainerJob binding, replacing tenant, Cluster UID, namespace and prepared
environment before unsuspending the Job. The independent console's Job submit
form continues to accept Raw only.

The controller creates one Task and an exclusive whole-host ContainerAllocation,
then pins the canonical Task target annotation. It does not write legacy
DockerBinding or count-based GPU reservations. The Job must contain exactly one
`docker.rlark.io/container` v1 template/carrier. Explicit image, entrypoint,
command, environment, working directory and CPU/memory limits are supported;
Pod templates, volumes, Secrets, environment injection, user GPU assignments and
Ray-specific fields are rejected. The allocation's complete GPU UUID set is
injected as NVIDIA DeviceIDs. One host runs at most one RLark Task at a time.

`Ready=True` means a fresh authenticated observation. Only an explicitly enabled
execution Agent advertises `docker-container-execution-v1`; an observation-only
node remains unschedulable. New allocation also requires current identity,
release readiness, uncordoned resources and an idle observation. Existing
allocations remain protected while busy or unavailable. The API revalidates the
exact Job/Task/allocation/Node, tenant, credential, generations and ownership
before claim and immediately before create/start. Agents receive no Kubernetes
credentials and cannot create allocations through the Task protocol.

`ExecutionKnown` records current bounded container phase. Natural exit first
records Succeeded/Failed with `ExecutionCleaned=False`, retaining its result and
allocation. The next cleanup step durably cancels, stops/removes and verifies
absence by ID, deterministic name and inventory; only then can
`ExecutionCleaned=True` and allocation `CleanupConfirmed=True` be published.
Suspension/deletion uses the same cleanup barrier without requiring readiness.
Terminal Task identities stay retained to prevent reruns after allocation release.

## Failure and recovery

The Agent holds an exclusive process lock on the private journal directory,
with context-aware serialization of Engine effects. It fsyncs cancellation before
stop/remove, and create intent immediately before ContainerCreate. An append-only
index detects missing individual records. A late request cannot restart a
cancelled Task UID, including after Agent restart or stop-before-create.

Inspect before create and recover an uncertain successful response using the
exact local creation record and ownership labels. Registry/pre-create failures
remain retryable. A create timeout with no container ID remains uncertain even
when Docker currently reports absence: retain the allocation. If the exact
container later appears, cleanup can remove it. No automatic timeout clears the
reservation. Corrupt/missing records, changed directory permissions, replacement
locks and Engine identity changes fail closed. Preserve the whole journal,
including cancellation records after release. Do not erase/recreate it to recover
an old node; loss of the entire directory requires operator fencing and exact
node retirement before registering a replacement.

Info/version/inventory/NVIDIA observation failures send no heartbeat; retry uses
bounded backoff and fresh reads. The controller expires readiness after 45
seconds, with periodic status repair. An Engine outage still permits a local
cancellation write but cannot produce cleanup proof. A replaced Engine requires
new registration/credentials after exact old-node retirement. Never force-delete
uncertain allocations or reuse an old credential.

## Bounded logs and resource snapshots

Diagnostics are disabled by default. Set package `enableDockerDiagnostics: true`
together with `enableExecution: true`, or add
`--enable-docker-diagnostics=true` to the execution Agent above. Keep
`enableLogReads: false`: that switch is for the Raw supervisor only. The Docker
Agent advertises `docker-diagnostics-v1` only after explicit enablement.

Configure the same API process that handles Agent polling with
`--enable-task-execution=true`, `--log-read-addr=127.0.0.1:9446` and
`--gateway-jwt-key-file=/private/gateway-jwt.key`. Configure the console with
`--log-api-origin=http://127.0.0.1:9446`. The loopback listener serves only the
administrator GET routes `/api/v1/container-runtime/logs` and
`/api/v1/container-runtime/stats`; both require exactly `namespace`, `allocation`,
`allocationUID` and `taskUID`. The browser's Gateway administrator JWT is checked
before waiting and before returning. The Agent uses a separate private mTLS
`/v1/docker-diagnostics` poll/complete endpoint; it never receives that JWT.
Raw retains its existing `/v1/logs` endpoint and supervisor semantics.

Each Docker node permits one pending diagnostic read, with at most 32 Docker
reads per API process and a 20-second wait. Pending reads disappear on API
restart; retry manually. Poll and read listeners must use the same API instance:
there is no cross-instance broker routing. The independent Agent worker bounds
each poll/read/complete step to ten seconds. Response-body reads do not hold the
execution gate, so a stalled read cannot prevent cleanup.

Every boundary revalidates the exact allocation/Task/Job/binding/Cluster/Node,
tenant and credential. Task generation changes, cleanup confirmation, release,
revoked capability and replacement identities reject reads, including results
already buffered. Cordon or stale heartbeat alone does not revoke retained
ownership. The local reader checks Engine ID, retained journal creation record,
Task generation, container ID and ownership labels both before and after reading.
Local cancellation blocks further reads; cleanup racing a read discards its data.
Unknown ownership, TTY containers, missing records or malformed responses return
unavailable without exposing local error paths. Workload output itself may contain
secrets: only authorized administrators should enable and use this preview.

Logs combine stdout/stderr in Engine order, without stream labels or follow.
They return the beginning of currently available Engine output, at most 64 KiB
of decoded bytes, with truncation metadata. Framing is bounded to 1 MiB and 4096
frames. This is neither a historical archive nor a tail API. Resource snapshots
are one Linux Engine sample with `sampledAt` and optional canonical unsigned
64-bit decimal strings: `cpuUsageNS`, `memoryUsageBytes`, `memoryLimitBytes`,
`pids`, `blockReadBytes` and `blockWriteBytes`. Missing counters remain absent;
zero is reported only when observed. CPU time is cumulative nanoseconds, memory
and block I/O use bytes, and PIDs is a count. The stats response body is bounded
to 1 MiB. One sample does not provide CPU percentage; GPU utilization and
host-network traffic attribution remain unavailable. Neither logs nor snapshots
are persisted in resource Status or a metrics database.

The independent console provides manual reads on allocation rows. Docker reads
require the advertised diagnostics and execution capabilities and an allocation
that has not been cleaned. Unknown counters show as unavailable; output renders
as escaped text. Selection changes, logout, allocation replacement/cleanup and
capability revocation clear displayed data and cancel stale requests. An API
restart, daemon outage or unavailable exited-container stats requires a new read;
no zero sample or automatic retry is synthesized.

## Validation and remaining scope

Tests exercise fake Engine response loss, immutable configuration, ownership,
whole-GPU IDs, cancellation, process locking, restart, outage/recovery, and real
mTLS plus official SDK Unix-socket transport through Running, success, failure,
suspension and cleanup. The daemon in these transport tests is simulated; no
real Docker GPU run is claimed. Diagnostics tests additionally cover bounded
multiplex decoding, absent/zero/overflow counters, exact-ID SDK reads, credential
and capability revocation, transient broker restart/retry, and cleanup while a
response body is stalled.

GPU utilization, port reservations, Ray/RLinf group barriers,
Docker rolling upgrades, and real GPU-host lifecycle acceptance remain separate
work. Host networking alone does not establish a multi-host Ray contract. See
[Docker execution plan](docker-runtime-execution-plan.md) for the ordered gates.


## Ray runtime foundation scope

The extension contains a tested internal Ray 2.56.1 compiler/launcher and
create-without-start provider primitive. They do **not** enable group dispatch:
the scoped Agent still installs only the single-container adapter. `rayPorts`
and resolved membership/GPU fields are internal allocation material and are
rejected in public container Task input. See the [Ray implementation contract](docker-runtime-execution-plan.md)
for the port block, prepare/start barriers, required image, failure/recovery tests
and remaining real-host acceptance. No new operator flag enables partial groups.
