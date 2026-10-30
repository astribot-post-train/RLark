# Raw runtime for fixed GPU containers

Status: implementation in progress; this is a delivery plan, not a statement of supported capabilities.

## Agreed scope

Existing GPU containers join one logical Cluster as independent Nodes. An Agent runs inside each container and executes processes in its preinstalled environment. There is no Docker daemon, Docker socket, container creation, or Docker-in-Docker requirement. One container is exclusively reserved for one Task at a time; a Job may occupy several containers. The first workload is Ray/RLinf within one cloud container group. Cloud-to-robot hybrid Jobs are outside this release. Container private addresses are mutually reachable; control-plane transport addresses are separate.

## Contracts and ownership

Reuse Cluster, Node, Job and Task. Cluster.spec.agentNamespace is authoritative membership; a label is not ownership. Persist node UID, container incarnation, execution and allocation identities. Agent owns observed capability/environment/health; the controller owns reservations. Introduce canonical runtime-neutral replicas, preserve unambiguous Kubernetes legacy reads and reject conflicts. Expand Raw replicas into independent Tasks with stable indices/ranks and aggregate both template and instance status. Node-scoped claimants must never claim siblings; direct Tasks must not bypass reservations or tenant checks.

## Scheduling and execution sequence

Use API resourceVersion compare-and-swap for exclusive per-node reservations. Acquire the full group incrementally, recover reservations after controller restart, and roll back incomplete acquisition with bounded backoff. Never launch a partial group. Retain reservations on disconnect, stale heartbeat or uncertain cleanup. Release only after positively confirmed cleanup. Manual retry creates a new execution identity after the previous group is cleaned.

The sequence is reserve all -> preflight all -> start head -> join workers -> verify exact assigned Ray membership -> start training once -> record result -> clean all. Configurable preparation/readiness deadlines fail closed. Worker/head failure stops the entire group; disconnect means unknown, not confirmation of failure or termination. User stop is distinct from failure. A successful training process determines success, not the lifetime of a background Ray worker.

## Runtime implementation

Implement a typed Raw process Provider and Ray-on-Raw Adapter in the existing Host/Coordinator framework. Inject process, filesystem, clock and API dependencies through small interfaces. No second execution controller outside these abstractions. Persist execution manifests before external effects. An independent supervisor survives Agent restart, persists outcome and is reattached using execution identity plus boot/start-time identity, never PID alone. Use isolated execution directories, process sessions, Ray temporary paths and bounded logs. Kill only proven owned descendants; never global ray stop or pkill. Ambiguous ownership, supervisor death with possible descendants, and PID reuse block reuse. Container replacement cannot adopt an old execution silently.

## Ray and networking

Use explicit private node IP and configurable disjoint Ray port ranges per network sharing scope. Disable unused dashboard/client facilities. Validate supported Ray versions and exact assigned membership, propagate readiness errors, and treat actual bind failures as execution failures. RLinf/NCCL connectivity must be tested separately; arbitrary framework-selected ports are not assumed controlled. One Task per container does not itself isolate host-network port namespaces.

## Environment, capacity and storage

Use preinstalled Python/Ray/RLinf only; validate absolute paths, versions and required capabilities. Do not install dependencies implicitly. Report cgroup/affinity limits rather than physical-host capacity. Visible GPUs are not authorized GPUs: require confirmed UUID allowlists/platform binding and reject unknown scope. Use whole exclusive GPUs only. Measured utilization is separate from allocation; telemetry is optional and bounded.

Use preexisting mounts only, validate access on every Node, and separate node identity/state directories from execution/log directories. Checkpoints are retained on cleanup/deletion. Rotate stdout/stderr locally and expose tail/since/follow through the authorized Gateway/Server/Agent route. Do not accept arbitrary file paths or PIDs. Credentials are node-scoped and excluded from workload process environments.

## Operator and UI experience

rlarkadm generates configuration and a foreground start package; integrate with an available supervisor without assuming systemd. Show Raw capabilities, environment, health and busy state. Job forms select Nodes, environment and entrypoint instead of images. Expose reservation/preflight/Ray/training/cleanup conditions, per-replica logs and manual stop/retry. Terminal/SSH injection, TensorBoard proxy, dynamic PVCs and cloud-provider scheduling adapters are excluded.

## Delivery checklist

- [x] Feature branch based on the approved parent branch.
- [x] English and Chinese design/delivery plan.
- [ ] Typed API, schema/deepcopy generation, compatibility and ownership tests.
- [ ] Durable exclusive reservation, rollback and recovery.
- [ ] Single-node supervised Raw execution through Host/Coordinator.
- [ ] Multi-node Ray barriers and group lifecycle.
- [ ] Node registration, credentials, capacity and environment validation.
- [ ] rlarkadm package, authorized logs, Gateway and UI integration.
- [ ] Examples, runtime capability/deployment/operations documentation in both languages.
- [ ] Focused and repository Go/web/doc checks; draft PR with evidence.

## Acceptance and failure tests

Cover competing reservations, partial rollback, controller restart, crash after external effect, Agent reattach, PID reuse, supervisor loss, orphan descendants, busy ports, unknown GPU/environment, wrong node/tenant claims, head/worker failure, network loss, stop versus failure, retained checkpoints and Kubernetes compatibility. CPU fake/process tests run locally. Real two-container Ray/GPU and minimal RLinf tests require prepared environments and explicit authorized GPU scope; do not silently install dependencies or launch cloud training. Record unverified behavior explicitly and keep maturity preview until end-to-end evidence exists.

## Current implementation evidence and remaining integration

This branch currently provides internal building blocks, not an operational Raw Agent. `agent --agent-type=Raw` does not yet wire these components. Do not register production GPU nodes or submit training Jobs on the basis of the new schema alone. The generated `status.raw`, `status.reservation` and `spec.placement.binding` fields are experimental contracts; credentials and server-side status-field ownership must be implemented before exposing them to node identities.

Implemented boundaries:

- `rawallocation.Manager` uses fresh reads and optimistic status patches for whole-node group reservations, incremental commit, same-execution recovery and rollback of uncommitted claims. The environment is frozen in the reservation. Group validation checks current membership, node/container identity, heartbeat, cordon/drain, tenant, private network scope, distinct training IPs and non-overlapping GPU UUIDs within the group. It does not yet prove GPU exclusivity across separate groups or platform allocations.
- `AssignTask` assigns an already committed reservation to one Task UID, idempotently. Allocation commit alone never authorizes an execution. Every node must have an assigned Task UID before node admission accepts the complete group. Admission also freshly verifies that every peer Task exists, is unsuspended, has its exact binding, and belongs to the same authoritative Job instance.
- `raw.NodeAdmission` re-reads the Task, owning Job and all reservations. It rejects direct Tasks, stale generations, suspension, replacement containers, wrong tenant and incomplete groups. The caller must use an uncached privileged management client; production credential/RBAC wiring remains pending.
- `raw.Provider` implements the existing Runtime Provider and inventory contracts. It delegates starts and cleanup to an independent supervisor; unconfirmed cleanup remains in progress. Raw Host claimant identities require the exact node UID and never adopt legacy claims.
- `raw.NewCoordinator` and `raw.NewHost` assemble explicit workload adapters, versioned process-carrier decoding, node-scoped claims, execution and status through the existing framework. Process manifests contain typed API metadata whose namespace/name/UID must match the Task; validation and lowering have no process effects. Host restart, duplicate reconciliation, sibling-node isolation and suspension cleanup are tested. No built-in Ray adapter, watch/resync loop or production entrypoint is registered. Unknown observations project to `Pending` with `ExecutionKnown=False` and the observed Task generation; this does not prove the process has stopped.
- The Linux supervisor persists intent before starting, uses atomic cgroup placement, records results, and only accepts Task identity on a private Unix socket. Agent client reconnection is tested. A supervisor restart marks uncertain persisted work Unknown and blocks reuse; it does not adopt PIDs.

### Experimental supervisor prerequisites and build

Build with `make -C apps/rlark raw-supervisor`. This target is opt-in and does not imply Raw Agent support. The service requires Linux with cgroup v2, `cgroup.kill` and kernel support for atomic `CLONE_INTO_CGROUP` placement. The operator must provide a writable delegated subtree and a distinct nonroot workload UID/GID. Workload processes must not be able to migrate to other cgroups, write cgroup control files, read node credentials, or write supervisor state/socket paths. An ordinary cloud container may lack these permissions; test delegation first, without giving the container a Docker socket or assuming that privileged mode is acceptable.

The strict JSON configuration passed to `bin/raw-supervisor --config /private/supervisor.json` uses these fields:

```json
{
  "stateDirectory": "/var/lib/rlark-supervisor",
  "executables": ["/usr/bin/python3.12"],
  "workDirectories": ["/workspace/training"],
  "logBytes": 10485760,
  "cgroupDirectory": "/sys/fs/cgroup/rlark-workloads",
  "workloadUID": 10001,
  "workloadGID": 10001
}
```

Paths must exist and be canonical absolute paths, including resolving executable symlinks before configuration. The state directory is private (0700), outside workload paths, and retained across Agent restarts. No Agent/supervisor environment is inherited by training. Authorized executable files and their parent directories must remain protected from workload replacement; path allowlisting alone does not establish file integrity. The service accepts an explicit environment and runs the configured executable as the workload UID/GID. The Unix socket is 0600; only the trusted local Agent identity may use it. Authentication/authorization for remote logs is not supplied by this socket.

Current logs retain only the first `logBytes` bytes per execution and discard excess while draining process output; rotation, tail, since, follow and Gateway routing are not implemented. Records and per-execution cgroup directories are not automatically garbage-collected. Do not remove active or uncertain records to bypass ownership checks. A supervisor stop/restart while work is active can leave uncertain descendants; use container/platform lifecycle evidence and an audited recovery procedure before provisioning a fresh node incarnation. The current code deliberately has no force-release/recover endpoint. Checkpoints outside private supervisor state are not deleted by this supervisor.

### Ordered integration work

1. Complete production ownership enforcement: scoped bootstrap/enrollment, unique Node/incarnation persistence, narrowly scoped status writes, leases, capacity from cgroups/affinity, and authoritative GPU binding across all groups. Prove that two agents cannot impersonate the same Node.
2. Add runtime-neutral replica validation and legacy conflict handling; expand Raw templates into stable per-replica Tasks. Create Tasks suspended/unbound, assign their UIDs, persist all bindings, then permit dispatch. Job-controller restart must reconstruct this sequence from current state.
3. Assemble the Raw decoder, Ray adapter, provider, lifecycle and status mapping through Host/Coordinator. Reject unsupported Kubernetes environments, carriers and Pod-template features. Keep the existing Kubernetes path unchanged and covered by regression tests.
4. Implement typed Ray configuration, preflight/ready barriers, bounded deadlines, per-network-scope port reservations, exact membership and training result reporting. No training before every assigned member is verified. Distinguish Unknown from failed, and stop all assigned members on confirmed group failure.
5. Complete cleanup/retry ownership: retain reservations until every supervisor confirms cleanup, including Job deletion/restart and partially dispatched groups. Add auditable operator recovery for supervisor loss; absence of a heartbeat never releases capacity.
6. Add the foreground enrollment/start package, authorized log route and bounded retention, resource observations, UI capability gates and per-replica progress. Add executable EN/ZH deployment and training examples only when this route works.
7. Run two prepared containers with the actual delegated cgroups, authorized GPU UUIDs and supported Ray/RLinf versions. Verify busy ports, descendant cleanup, Agent restart, supervisor loss, network loss, head/worker failure and retained checkpoints. These platform tests have not been run locally.

### Local validation evidence

The internal packages pass focused race tests and the repository `make test-go` suite. The supervisor binary builds, generated API/client/UI models have been regenerated, and the paired documentation passes a strict MkDocs build. No real GPU, delegated-cgroup or two-container Ray/RLinf acceptance run has been completed. Repository Go lint still reports three pre-existing unchecked channel receives in network packages; the new Raw packages have no reported lint findings. This evidence validates the experimental foundation only and does not complete the delivery checklist above.

### Kingsoft Cloud container preflight (2026-10-30)

The previously identified development container was inspected over its existing SSH connection. Only short CPU processes and capability probes were run; no GPU computation, dependency installation, platform task changes, parent cgroup writes or filesystem remounts were performed.

| Probe | Observed result | Consequence |
| --- | --- | --- |
| Kernel and cgroup | Linux 5.15; cgroup v2 mounted read-only | Creating a child cgroup fails with `EROFS`; the current supervisor cannot run on this container. Presence of `cgroup.kill` alone is insufficient. |
| CPU and memory boundary | CPU affinity: 192; `cpu.max`: `12000000 100000`; `memory.max`: `1523639648256` | Affinity alone overstates effective CPU capacity; the quota is 120 CPU equivalents. |
| GPU visibility | `nvidia-smi` lists six H100 80GB devices | Device enumeration works, but visibility does not establish allocation authorization. No GPU execution was attempted. |
| Default Python | `ray`, `torch` and `rlinf` distributions absent from the inspected interpreter; no `ray` on PATH | This interpreter is not a prepared training environment. Other custom environments have not been exhaustively searched. |
| CPU subprocess | A short Python child exits successfully | Ordinary process execution works. |
| User/PID namespace | `unshare --user --map-root-user --pid --fork` succeeds | A possible alternative isolation boundary exists; it is not implemented by this supervisor. |
| Namespace descendant cleanup | Namespace PID 1 exits; its child creates a new session; parent verifies no surviving non-zombie processes by PID/start time | The kernel cleaned up the tested CPU descendants despite `setsid`. This is a feasibility probe, not Ray/GPU acceptance or a production ownership/recovery implementation. |
| Private proc mount | Combined user/mount/PID namespace with `--mount-proc` fails on filesystem propagation with `EPERM` | Do not assume mounting a private `/proc` is permitted. A namespace implementation needs a separate design and validation. |

The available notebook-list response did not identify this SSH endpoint, so its platform GPU entitlement remains unverified. Do not infer an authorized GPU allowlist from the six visible UUIDs. The current cgroup supervisor is blocked on this target. Continue production integration only after an execution boundary compatible with the actual container is implemented and tested; do not silently fall back to process-group-only cleanup, privileged mode, or host cgroup remounting.
