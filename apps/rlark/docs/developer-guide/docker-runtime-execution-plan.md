# Docker runtime execution plan

This document is the implementation contract for the independent
`extensions/container-runtime` Docker path. It follows the existing
ContainerCluster/ContainerNode/ContainerJob/ContainerAllocation resources and
keeps Docker-specific code in the extension module.

## Target behavior

A Docker Cluster is a separate logical resource from a Kubernetes Cluster. Each
registered Linux Docker host is one ContainerNode. The control plane selects a
whole host and persists one exact ContainerAllocation before dispatch. The
Agent receives only the allocation-bound Task protocol over mTLS; it connects
to the host Docker Engine through a local Unix socket and never exposes that
socket to a workload container.

The first executable contract is one immutable Docker container per Task:

- `image`, command, entrypoint, environment and working directory are supplied
  by the versioned Docker carrier.
- host networking is required for private multi-host training traffic.
- one allocation owns the complete authorized GPU set; a task cannot choose a
  different UUID list.
- the provider uses deterministic labels containing Task UID, namespace, name,
  runtime and incarnation. It adopts only an exact matching container with a retained local creation record.
- create, start, inspect, stop and remove are idempotent. A lost response is
  recovered by inspecting the deterministic name and labels.
- a retained exited/dead container remains occupied until its owner proves
  removal. Unknown containers are never adopted or deleted.

## Task and Ray dispatch

The existing controller expands a Job into independent Tasks. The Docker
scheduler uses the same durable per-host lock, but selects only Docker nodes and
writes the canonical Task target annotation plus the exact ContainerAllocation
before an Agent can start; it does not write legacy `DockerBinding`. A Docker Task is dispatched only when the node heartbeat is fresh, the
Engine ID and credential match, the target release is ready, and the allocation
UID/Task UID pair is current.

Ray/RLinf is a later gang layer. The current Docker execution contract supports one
explicit container Task per Job and rejects Ray groups, `rayPort`, user GPU
assignments and reserved `RLARK_*` environment fields. A later Ray adapter must
reserve its host ports in the same node allocation and reject collisions. A multi-node
Ray Job is admitted only after all member containers report current private
addresses and the same Job generation. Until that barrier exists, the API must
reject Ray group dispatch instead of starting a partial group.

## Agent protocol and ownership

`assignment`, `get`, `authorize`, `claim`, `status` and `cleanup` are the only
Agent actions. The API revalidates the native Job, Task, Cluster, Node,
ContainerNode, allocation, tenant, generation and credential for every action.
The Agent has no Kubernetes credentials. The Docker provider's admission callback
reuses the server-side authorization result immediately before `ContainerCreate`.

Status separates desired state, observed container state and cleanup proof:

- `ExecutionKnown=True` reports a bounded Docker phase and exit reason.
- `ExecutionCleaned=True` is written only after an owner-scoped stop/remove and a
  subsequent inspect/list check proves the container is absent.
- logs and stats are fetched only for the exact container ID while the allocation
  is current; they are not stored in resource Status.
- an Engine outage leaves the allocation protected and sends no synthetic
  heartbeat or cleanup proof.

## Rollout and upgrades

Docker and Raw packages share certificate and release verification, but runtime
flags are mutually exclusive. A Docker package starts after `docker.service`.
Docker upgrade execution is not implemented. Its future selector must choose
only an idle Docker node and preserve the same Engine socket and incarnation. A target release fences new allocations;
installation is an operation with an exact allocation and restart receipt.

## Acceptance gates

1. Fake Engine tests prove ownership, idempotent create/recover, immutable
   configuration, GPU request, host network, port collision, stop/remove and
   outage recovery.
2. Scoped protocol tests prove a Docker node can authorize and claim a Task
   without Kubernetes credentials, while Raw paths remain compatible.
3. An official SDK Unix-socket test proves the Agent talks to the local daemon.
4. A real private Docker GPU host must pass one image pull/create/start/inspect,
   logs/stats, stop/remove and restart recovery. This gate is separate from the
   current Raw-only Kingsoft Cloud notebook, which has no Docker socket.
5. Multi-host Ray/RLinf, gang barriers, port reservation and rolling upgrade
   acceptance remain separate gates after the single-Task lifecycle is stable.

## Local cancellation barrier

Execution requires a root-private persistent state directory. An exclusive process lock plus context-aware in-process
serialization protects create/start and stop/remove across all Task UIDs. Cleanup fsyncs a cancellation record before touching the Engine. Late
Agents, duplicate requests, process restart and stop-before-create cannot restart
that Task. Records and an append-only index survive allocation release; corrupt/missing
records or changed permissions block execution until operator recovery. Entire
directory loss requires fencing and retiring the old node; never recreate an
empty journal for old credentials. The first carrier uses
explicit `docker.rlark.io/container` v1 parameters and rejects user-provided GPU
UUIDs, reserved ownership labels, Pod templates and unsupported Ray groups. The
allocation injects GPU UUIDs independently through NVIDIA DeviceIDs.

## Implementation sequence

1. Wire Docker admission into the scoped Agent Task loop and add provider
   lifecycle/status tests.
2. Add exact Docker logs/stats API actions, bounded console routes and failure/recovery tests.
3. Add scheduler port reservations and reject incomplete Ray groups.
4. Add Docker Agent release rollout selection and a real-host acceptance test.
5. Add the Docker cluster management UI for runtime, Engine identity, node
   inventory, occupancy, capabilities, heartbeat, rollout and blocked reasons.

## Implemented milestone (2026-10-31)

The first implementation step is complete as an opt-in preview: single-container
adapter/strict contract, companion scheduling, mTLS authorization/claim/status,
exact GPU UUID injection, private journal/index and cancellation barrier,
root/systemd package flags, terminal result/cleanup separation and restart
recovery. The default Agent/package still observes only. Controller reservations,
API execution and Agent execution must all be enabled explicitly. See the
[operator guide](docker-agent-observation.md) and executable templates under
`extensions/container-runtime/config/examples/docker-container-*.yaml`.

The tests include fake daemon failure/recovery, concurrent cancellation,
interprocess lock contention, missing record detection, the official SDK over a
Unix socket and mTLS through success/failure/suspension plus exact allocation
cleanup. They do not establish real Docker GPU acceptance. Bounded Docker diagnostics are now implemented as a separate opt-in preview.
Port/gang coordination, rolling upgrades and the Docker console Job submission
flow remain subsequent implementation steps. Existing Raw tests
and cloud acceptance retain their original scope.

## Bounded diagnostics contract

Docker diagnostics use a dedicated bounded broker and the private
`/v1/docker-diagnostics` mTLS poll/complete transport, with explicit `logs` or
`stats` kinds. Raw retains its existing `/v1/logs` protocol.
Docker nodes must explicitly enable `--enable-docker-diagnostics=true` together
with execution and advertise `docker-diagnostics-v1`. Observation-only Agents
cannot read workload output. The package field is `enableDockerDiagnostics`.

Administrator GET routes are `/api/v1/container-runtime/logs` and
`/api/v1/container-runtime/stats`. Both accept exactly `namespace`, `allocation`,
`allocationUID` and `taskUID`. The existing loopback read listener and console
origin serve only these fixed routes. Each request permits one pending read
per Docker node and 32 Docker reads globally, waits at most 20 seconds, and rechecks administrator
credentials and exact allocation/Task/Job/cluster/node/tenant/credential ownership
before returning. Requests disappear on API restart and may safely be retried.
Released or cleanup-confirmed allocations, changed Task generation, revoked
credentials and disabled capability deny reads. Cordon and suspension alone do
not revoke retained ownership; local cancellation does stop Docker diagnostics.

The local reader pins the journal's exact container ID, Task generation,
Engine incarnation and labels before and after a read. It must not hold the
execution gate while reading the Engine response body. Each diagnostic worker
step has a ten-second context bound and runs independently from reconciliation.
Logs combine stdout/stderr in Engine order, without follow or stream labels,
return at most 64 KiB of decoded bytes, and bound multiplex framing work. They
are a preview of available Engine logs, not a historical archive or tail API.
Unknown ownership, missing records and malformed responses return unavailable.

Resource snapshots expose a UTC sample timestamp and optional canonical unsigned
64-bit decimal strings: CPU cumulative nanoseconds, memory usage/limit bytes,
current PID count, and block read/write bytes. Missing fields remain absent,
never synthesized as zero. The stats JSON body is limited to 1 MiB. A single
sample does not imply CPU percentage; GPU utilization and host-network traffic
attribution are unavailable. Logs and snapshots are transient, never resource
Status or persistent metrics. The console provides manual reads, escaping
output and discarding in-flight results after selection changes or logout.


## Ray/RLinf implementation contract (2026-10-31)

This section is the next implementation plan, **not enabled Ray scheduling**.
The scoped Agent continues to install only the single-container adapter until
all group admission, failure and recovery gates below pass. The old unscoped
Ray adapter must not be enabled as a shortcut. Changes stay in the independent
extension; native Cluster/Node/Job/Task APIs remain unchanged.

### Ports and host ownership

One member exclusively owns one host and its authorized whole-GPU set. One
host therefore runs at most one RLark member at a time, while one logical
Cluster can run several Jobs on disjoint hosts. Host networking does not limit
a whole logical Cluster to a single Job. Co-location and GPU slicing remain
outside this contract.

Replace the head-name hash with one explicit versioned port block, chosen once
for the execution and copied identically to every member's immutable allocation.
The initial block has nine service ports and 1..256 worker ports; it includes
GCS, node manager, object manager, Ray client, dashboard HTTP, dashboard
agent HTTP/gRPC, runtime-environment agent and metrics. The launcher pins all
of these Ray services and the worker range. Non-Ray training traffic (for
example NCCL and user rendezvous ports) requires an operator-defined private
network policy; the Ray block is not a complete training firewall allowlist.

The local Docker provider checks the full declared TCP list before create and
again before starting a retained `created` container. The port list is immutable
execution material and must survive inspect/recovery. Check wildcard IPv4 and
IPv6 listeners and reject collisions; never kill or adopt the occupying process.
Sockets are released before Docker starts: this is a preflight check, **not a
race-free kernel reservation against foreign processes**. A dedicated host and
an operator-reserved port block remain prerequisites. RLark's durable whole-host
allocation provides exclusivity only among RLark workloads. An already running
owned container must not fail recovery because it occupies its own ports.

### Replica expansion and durable group identity

The Job controller consumes the Docker carrier's template replica count and
creates one native Task per replica, with exactly one head replica. Canonical
child names contain template and replica index; ranks are stable (head first,
then template name and replica index). The complete set, bounded initially to
16 members, is validated before creating any Task. Mixed runtimes, multiple
heads, user-resolved head addresses/ranks/GPUs/ports, unsupported fields and
ambiguous ownership fail before dispatch.

Persist an identifiable execution operation for the exact binding UID, native
Job UID/generation and restart identity, with immutable member Task UIDs and
chosen node/port plans. It is not an edge-triggered `start` boolean. Per-host
ContainerAllocations remain the exclusive locks shared with upgrades. Their
Task targets retain operation/member/port identity. The API has no multi-object
transaction: partial reservation is expected and must be reconstructable after
restart. Competing Jobs must not accumulate partial locks indefinitely; an
incomplete attempt is cancelled durably and released only after every exposed
member confirms no execution/cleanup. A lost allocation-create response remains
uncertain and must not cause fallback to another host. Fairness, preemption and
atomic simultaneous startup are not promised by the first group scheduler.

### Prepare, start and training barriers

1. **Allocated:** every exact member has an allocation at the current Job
   generation, same tenant, logical Cluster and network scope, unique host/IP/GPU
   identity, required Agent capability and fresh authenticated heartbeat.
2. **Prepared:** Agents pull/verify images, preflight ports and create containers
   without starting them. Each returns a bounded receipt pinned to operation,
   Task UID/generation, allocation UID, Engine ID, container ID and manifest
   digest. The private journal records identity before publishing the receipt.
   A crash or lost response recovers the exact container; it never adopts a
   similarly named container or recreates an uncertain create.
3. **Start permitted:** only after all current prepared receipts match may the
   API authorize each container start. Every create/start boundary revalidates
   the full group and local durable cancellation. This permits coordinated
   startup, not an atomic distributed start. Changing/suspending/deleting the
   Job or losing group ownership revokes subsequent starts.
4. **Ray ready:** the head and workers run a trusted launcher in the workload
   image. It starts Ray without putting `--block` in front of a user run script,
   waits for the exact expected private node addresses, and only then runs the
   head training script. Workers remain supervised resident members; a missing
   member or timeout fails startup. Container `Running` alone is not Ray ready.

The initial internal launcher requires Ray CLI/SDK **2.56.1**, Python 3 and a POSIX shell for
explicit user scripts. The launcher uses argv for generated commands and
preserves literal arguments; only prepare/run scripts are shell boundaries.
It must not execute an image default command accidentally. Cleanup targets
only that container's process tree and never calls host-wide `ray stop`.
A prepare script runs in a child shell and does not export environment changes
back into the launcher. User run and container command are mutually exclusive;
the first group contract runs training on the head only.

### Failure, completion and recovery

Persist a group stopping/failure observation before stopping siblings. An
unexpected member exit (even a clean worker exit), invalid receipt, startup
failure or explicit stop fences further starts. A transient API/Engine outage
retains locks; heartbeat expiry does not prove process absence or free GPUs.
Head success is the training result; it still requires orderly worker stop and
all exact container removals before releasing the group's allocations. Keep
terminal execution and Task identities so ordinary reconciliation cannot
silently rerun a completed or failed group. Restart is an explicit new execution
identity, admitted only after all previous members have cleanup proofs and
allocation absence is observed.

Every protocol action is scoped by mTLS node identity and exact operation,
allocation and Task UIDs. No Agent receives management Kubernetes credentials.
Status observations do not authorize adoption of replacement resources. A
replaced Engine/node/credential, lost journal, missing member identity or
uncertain create requires explicit recovery; never force-release automatically.

### Delivery and acceptance order

1. Replace the dormant Ray compiler's hash/shell launcher with explicit typed
   port/assignment input, complete port flags, supervised readiness and literal
   argv; enforce provider preflight and recovery. Test collisions, occupied
   ports, malformed inputs, cancellation, worker failure and restart. This
   runtime foundation does not advertise Ray dispatch.
2. Implement replica expansion and the immutable execution operation, partial
   reservation cancellation, exact prepared receipts and start authorization.
   Test two competing groups, stale generations, lost creates/receipts, controller
   and Agent restarts, unauthorized peers and cleanup before capacity reuse.
3. Connect group failure fan-out, terminal retention and head/worker readiness;
   add mTLS integration tests with all members, including scarce capacity and
   foreign port listeners. Keep independent Raw semantics unchanged.
4. Validate two real private Docker GPU hosts with the same Ray version and
   image: distributed CUDA/RLinf smoke run, logs/stats, stop, worker loss,
   Agent restart, cleanup and a second Job reusing capacity. The Raw notebook
   and local CPU-only Ray tests cannot substitute for this gate.
5. Expose typed group phases, member placements, reserved ports and blocking
   reasons in the independent console, then add submission/registration flows.
   Docker idle upgrades remain a separate acceptance gate.


### Runtime foundation delivered

The internal compiler now requires a trusted typed Assignment (exact Task
UID/generation, ordered private member addresses/rank, GPU UUIDs and PortPlan).
An empty or mismatched assignment fails; there is no hash/name/hostname fallback.
Only an explicit supported PodTemplate/container subset is accepted. CPU count
must be whole (the tested Ray CLI accepts integers); GPU requests must match the
complete assigned set. Scripts and command are mutually exclusive, training is
head-only, and image default entrypoints/commands are never inherited.

The nine-service contiguous block and worker range become immutable manifest
material in `rayPorts`, retained under an extension-owned container label and
protected by the journal digest. The public single-container Task validator
rejects this internal field. No allocation CRD/Agent protocol or advertised
capability has been expanded yet; operation-pinned allocation material and
prepared/start receipts remain the next delivery gate.

`ScopedProvider.Prepare` records/recovers one exact created container without
starting it. A later `Apply` rechecks ports and current admission immediately
before start. Preparation and cleanup share the cancellation journal/lock; lost
create results, cancelled identities and immutable port drift preserve existing
failure semantics. An already running container is observed without probing its
own occupied ports. A failed preflight before create does not mark create
uncertainty. IPv6 socket support is required for the dual-family check; an
unavailable family fails closed.

The Python launcher starts `ray start --block` as a supervised child while its
own process continues readiness and training. It queries the version-pinned GCS
protobuf API with a two-second call deadline and 1 MiB response bound, checking
the exact alive private address set for up to 120 seconds. It does not create a
Ray driver for readiness. The trusted private network must restrict Ray access;
this address check is not authentication or network isolation. The launcher
preserves prepare/training exit codes, treats every spontaneous worker exit as
failure, and sends TERM then KILL only to its child process groups. Docker PID-1
exit terminates descendants that moved into different groups. This launcher is
for Docker PID namespaces, not a host process backend.

Tests cover real IPv4/IPv6 listener conflicts, prepare/restart/start/revocation,
lost create, cancellation, port-label drift, strict assignment/template/resource
validation and ten Python process cases. The process cases use fake Ray/GCS
modules to exercise real subprocesses, signals, literal argv, membership waits,
timeouts and cleanup; they are not real Ray cluster/GPU acceptance. The tested
Ray 2.56.1 CLI help was separately checked for all generated flags in an isolated
Python environment. Ordinary Go tests invoke the process suite when `python3`
is available (otherwise that suite explicitly skips). Broader Ray versions and
real multi-host training remain unaccepted.
