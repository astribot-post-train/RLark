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
2. Add exact Docker logs/stats API actions and bounded console routes.
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
cleanup. They do not establish real Docker GPU acceptance. Scoped Docker
logs/stats, port/gang coordination, rolling upgrades and the Docker console Job
submission flow remain the subsequent implementation steps. Existing Raw tests
and cloud acceptance retain their original scope.
