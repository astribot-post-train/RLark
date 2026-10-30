# Container cluster extension: design and delivery plan

Status: single-node Raw preview deployed, GPU lifecycle accepted, and private-cloud idle rollout accepted on 2026-10-31. The independent administrator console now has a preview; see [Container runtime management console](container-runtime-console.md). Multi-node Ray/RLinf, native UI integration, failed-exec rollback and power-loss recovery remain incomplete. Bounded authenticated Raw log transport is an opt-in preview with private-cloud end-to-end acceptance on 2026-10-31. Historical evidence below is separate from the current extension acceptance.

## Approved architecture and delivery order

Write and validate this English plan and its Chinese counterpart first, then implement in the existing feature branch and PR without rewriting published history. New code belongs in `extensions/container-runtime/`, with an independent Go module, API group, UI package, binaries, tests and build entry point. Reuse native Cluster/Node/Job/Task as the inventory and workload model. Do not add container-specific fields to their target schemas. Existing preview fields remain compatibility data until a safe migration proves cleanup and ownership.

Native RLark integration is generic only: controller ownership dispatch, dependency injection, service/UI routing and build wiring. Container scheduling, registration, resource observation, authorization policy, release distribution and runtime behavior belong in the extension. Reuse the exported Host/Coordinator/Provider/Adapter contracts rather than duplicating the execution engine. Keep generic changes independent from Docker/Raw implementation and maintain an integration inventory for upstream upgrades.

### Typed companion resources and ownership

The extension owns the `containers.rlark.io/v1alpha1` API group. `ContainerCluster` references one native Cluster by stable identity and UID and records runtime and rollout policy. `ContainerNode` references native Node and Cluster UIDs and records container/Engine incarnation, private address, capabilities, environment, authorized GPU UUIDs and Agent versions. `ContainerAllocation` records Job/Task UIDs, generation, tenant, node incarnation, execution identity, reserved devices and cleanup progress. `ContainerJob` is a typed controller binding to one native Job, not a second workload API. `AgentRelease` identifies an immutable artifact, SHA-256 digest and protocol compatibility.

`Cluster.spec.agentNamespace` remains authoritative membership. Tenant identity is validated independently of namespace and inherited by companion resources. Labels and annotations are lookup hints only. Dispatch verifies the typed binding, exact Job UID and tenant before selecting the extension owner. Active Job ownership is fixed while Tasks or cleanup remain. Direct Kubernetes API writes and extension REST requests have equivalent admission checks. Mixed Kubernetes/container Jobs are rejected in preview; native Kubernetes Jobs retain their behavior.

For each owned native Task, the extension persists one canonical scheduling target in the `containers.rlark.io/allocation-target` annotation before creating a `ContainerAllocation`. The annotation contains the Task UID, native Node UID, ContainerNode UID, Cluster UID and node incarnation. It is a durable scheduling intent, not proof of a reservation; the allocation remains the ownership lock. A retry or a second controller may only reuse the exact target, and must never select another node after an uncertain create or dispatch result. A target is cleared only after the allocation has a matching cleanup proof and the Task object is being replaced with a new UID. Task deletion, suspension and spec replacement keep the target and allocation identity until the Agent reports cleanup. Desired lifecycle is maintained from current state: `suspended` and deletion fence new starts, while heartbeat and Agent results are observations and never release a lock by themselves.

Raw supervisor stop persists an identity-only cancellation tombstone even when that Task has not started. A delayed start for the same Task UID must then fail, including after supervisor restart. Existing uncertain processes cannot use this absence path; they retain their allocation until positively cleaned. Failure to persist cancellation returns an error and cannot be reported as cleanup. Tombstones are retained without automatic garbage collection in this preview.

The opt-in preview Job reservation controller materializes and reserves one independent native Task per bounded `raw.rlark.io/process` v1 template in a Job (one to sixteen uniquely named templates). It rejects Ray, legacy bindings, explicit runtime instance names, SSH injection, user GPU-visibility environment variables, environment handlers and Pod templates until their own dispatch contracts are implemented. Native Task names are deterministic; exact owner references, parent UID, template identity and tenant are verified without legacy adoption. Node selectors are validated and applied to native Node labels. Workload or restart changes replace the Task with a new UID after proven cleanup, rather than mutating a running execution. Suspension/deletion keep the Job and Task finalizers until every exact allocation is cleaned and observed deleted. A pinned target whose allocation is absent remains blocked unless the controller has already saved an exact cleanup receipt: absence alone cannot rule out an in-flight create or start. Before releasing an allocation, the controller persists `containers.rlark.io/allocation-cleanup-receipt` on the Task with the allocation UID and full target identity, allowing recovery after a lost deletion response. Orphan allocations block Job deletion; ambiguous data requires explicit operator recovery. Scheduling decisions use an uncached API client and optimistic concurrency. The controller aggregates the supported independent Raw Tasks: a current `ExecutionKnown=True` observation reports Pending/Running, and a current Succeeded/Failed result is published only after `ExecutionCleaned=True`, an exact cleanup proof and observed allocation deletion. Cleanup waits are exposed separately through `ExecutionObserved` and `AllocationReleased` conditions. Completed Task identity remains protected so controller or Agent restarts cannot rerun it; Job suspension/deletion, template changes and explicit restart retire it. Bounded authorized log transport is an opt-in preview. Ray/RLinf barriers, automatic enrollment/renewal and native UI remain incomplete; runtime-neutral multi-template expansion is implemented, while cross-member coordination is not. Explicit administrator onboarding and opt-in Agent upgrades are described below; private-cloud rollout acceptance is recorded below.

The standalone controller enables Kubernetes Lease leader election by default in `rlark-system` (`--leader-elect`, `--leader-election-namespace`). Use `config/rbac/container-runtime-controller.yaml` with the appropriate namespace and a management-API kubeconfig. The namespace must already exist. `--enable-job-reservations=false` is the default and installs validation controllers only; opt in to reservations with `--enable-job-reservations=true` for isolated preview tests. Suspend the whole Job with `Job.spec.suspended`; independently suspended templates are rejected in this slice. The optional in-process `ControllerExtension()` remains validation-only. Do not run the old preview allocation writer on the same nodes. Revoking or disabling the controller does not clean or release persisted reservations; restart it to resume reconciliation. Reservation tests cover recovery, unavailable capacity, changed configuration, deletion, tenant conflicts and lost API responses. They do not establish Agent execution or end-to-end training support.

The controller owns allocation intent and dispatch; Agents own observed runtime state and cleanup evidence. Persist reservations before dispatch with optimistic concurrency, recover from current state after restart, and never release on heartbeat loss. Each Raw container or Docker host accepts one Task at a time, and the current allocation contract requires an explicit authorized GPU allowlist. GPU allocation uses authorized whole-device UUIDs; visibility is not entitlement. Unknown cleanup blocks reuse. Logs, telemetry and audit records stay outside resource Status.

### Independent services and operator interface

The implemented execution preview adds an opt-in node-scoped Task channel to the standalone extension API. Verified Agent certificates are the only request identity; every read, metadata claim, status report, start authorization and cleanup report rechecks the current credential, companion/native UID references, independent tenant identity and exact allocation. The Agent receives only its own allocation and Task, with no native kubeconfig, discovery, watch, secrets, proxy or arbitrary resource operations. Task claim writes are limited to Host-owned annotations/finalizers and require the caller-observed resourceVersion; status writes preserve controller-owned fields and must match the current Task UID and generation. Runtime effects still go through the existing Host/Coordinator/Raw Provider and private supervisor. The generic Host metadata dependency is narrowed to its existing Get/Patch methods, allowing a real scoped transport without a general-purpose Kubernetes client. The execution API only validates an existing allocation; it cannot create locks or repair allocation targets. Task specs compare opaque JSON semantically, preserving exact numbers while tolerating API object key reordering.

Before each process start, the API checks unsuspended current Job/Task state, their allocated generations, the unique typed Job/Cluster bindings and the active credential. Node replacement, legacy reservation, cordon/drain, stale heartbeat, release mismatch or changed selector fails closed. The decoder injects `CUDA_VISIBLE_DEVICES` only from the immutable allocation's authorized GPU UUIDs and rejects user GPU-visibility overrides. Cleanup has a separate identity path: suspension, deletion and expired heartbeat block starts but must not prevent the exact owning Agent from reporting cleanup. Supervisor cancellation is persisted even for a never-started Task before any cleanup proof is accepted. An unavailable supervisor or unconfirmed descendants never imply cleanup. A transient API outage retries current state; it does not stop a running trusted workload or release its allocation. Each member still executes as one independent process; Ray groups, automatic enrollment/renewal, logs, cloud upgrades and UI keep their own acceptance gates.

Build independent extension controller/API, node Agent, supervisor and administration binaries. Nodes explicitly join a selected container Cluster using bounded bootstrap credentials and then use node-scoped authenticated outbound connections; no public IP or control-plane inbound connection is needed. Raw runs trusted root processes in the existing container. Engine mode uses a host Docker socket and never Docker-in-Docker. Advertise those capabilities separately.

Provide a dedicated Container Clusters page with independent UI/build packaging and a minimal RLark navigation entry. Show cluster identity/runtime/connection state, node Ready/schedulable/busy state, capacity/allocation/utilization separately, authorized GPUs, prepared environments, Jobs, Agent versions and rollout failures. Administrators can create/join clusters, generate onboarding packages, cordon/drain nodes and publish target releases. Job submission explicitly selects a Cluster and is checked server-side.

Control plane hosts immutable Agent artifacts. Nodes authenticate downloads, verify digest and platform/protocol compatibility, stage atomically and retain the previous version. A target release is durable; at most one idle node per Cluster upgrades at a time. Fence new allocations, health-check, and roll back on failure. Agent updates do not restart the workload supervisor. Support systemd hosts and a foreground root launcher/updater for notebooks without systemd.

### Bounded remote Raw log reads

The additive preview exposes on-demand log snapshots for the exact active allocation. An administrator request supplies the allocation namespace/name/UID and Task UID. The API revalidates native/companion node and cluster identity, tenant, Job binding and allocation ownership before queueing and again before returning bytes. The node polls over its existing authenticated outbound mTLS connection; no node inbound listener or Docker socket is introduced. Cordon, suspension and stale readiness do not by themselves revoke an otherwise exact log owner; credential revocation, replacement, allocation release or ambiguous ownership do.

Requests are ephemeral read operations, held only in the API process for at most 20 seconds, with one outstanding request per node and 32 globally. An API restart loses the request; the administrator retries the read. The API never stores logs in CRD Status, Events, audit payloads or application diagnostics. The supervisor accepts only an exact recorded Task identity, reads a regular private log file without following symlinks, and returns at most 64 KiB from the beginning of the retained combined stdout/stderr. Its configured retention still bounds the file. A truncation flag means additional retained bytes exist, while a retention-limit flag means the file reached its configured bound (not proof of dropped bytes).

The administrator read listener is explicitly enabled on a loopback address, validates the trusted Gateway administrator JWT independently, and is proxied by the independent console through a fixed read-only route. Agent endpoints continue to require mTLS. Logs may contain secrets printed by a workload, so they are restricted to administrators and rendered as plain text, never HTML or terminal control sequences. Historical logs after allocation release, archives, tail/since/follow, resource stats and terminal/exec remain unsupported. Local transport tests, revocation/replacement/race tests, bounded-request recovery tests and an opt-in real GPU job must pass before remote log acceptance is recorded.

### Migration and acceptance gates

The first independent control-plane slice uses a declared generic Job controller owner (`rlark.io/controller-owner`), verified against a typed ContainerJob binding. Native controllers fail closed for a declared external owner even when the extension is absent. The validation controller checks UID and authoritative namespace references; it does not authorize workload effects or mark a registered node schedulable. AgentRelease metadata validation never proves artifact availability. Extension CRD writes remain restricted to trusted administrators until equivalent admission is installed. Do not redirect live workloads to the companion API until allocation, scoped Agent transport and cleanup reconciliation are complete.

The extension now includes an API-backed exclusive per-Node allocation primitive. It takes the lock by creating one deterministic ContainerAllocation keyed from the native Node UID, checks authoritative membership, tenant, Node/Job/Task UIDs, Agent generation/readiness, heartbeat age, GPU allowlist, private address, duplicate GPU/address scope, and absence of legacy native reservations. Repeated acquire recovers only the exact same Task/allocation; a competing Task loses on create conflict. Release requires a status cleanup proof for the exact allocation UID and keeps the finalizer until deletion is observed. A standalone mTLS endpoint accepts `POST /v1/heartbeat` only from verified certificates carrying the container Agent role, validates the signed companion UID, tenant, credential, native bindings, private address and authorized GPU visibility, then patches bounded ContainerNode status. The Linux foreground Agent binary now observes the current cgroup-v2 boundary, visible GPU UUIDs and private Raw-supervisor occupancy, includes an explicit `busy` field, and retries transient observation/API failures with bounded backoff. It refuses to report a node when the supervisor or GPU observation is unavailable, so an outage becomes a stale heartbeat and scheduling fence. Controller repair preserves a fresh Agent observation and fences stale heartbeats; allocation checks the current credential identity and busy state. The API server binary and its narrow `config/rbac/container-runtime-api.yaml` permissions exist. Explicit administrator registration, offline certificate/package export and verified release distribution are implemented as opt-in preview capabilities described below. Raw Jobs may expand into independent member Tasks; automatic enrollment/renewal and Ray/RLinf multi-node lifecycle remain incomplete. TLS deployment, CA distribution and certificate issue/rotation remain operator-managed.

First copy and test existing Docker/Raw providers, allocation logic, supervision and tests in the independent module. Then replace preview bindings/reservations with companion resources, update decoder/admission and establish generic Job dispatch. Do not run old and new allocation writers simultaneously. Migration cordons legacy nodes, waits for verified cleanup, creates UID-checked companions and enables the new owner only after validation; ambiguous active data remains blocked and visible.

First cloud acceptance is one prepared Raw node and one GPU process Job: explicit Cluster selection, registration, scheduling, GPU use, bounded authorized logs, success/failure/stop, confirmed descendant cleanup, retained checkpoints and Agent upgrade/rollback. Multinode Raw Ray/RLinf, cross-runtime Jobs, terminal/exec, dynamic storage and cloud auto-provisioning remain later work. Preserve tested Docker lifecycle behavior, but do not advertise multinode GPU training, logs/stats or GPU UUID assignment before dedicated acceptance tests.

### Kingsoft Cloud test machine

The user selected queue `regular-training` in resource pool `GPU-Pool-Regular-Training`, not `regular-dev`. Company guidance normally places 0/1-GPU notebooks in Dev queues, so record this explicit user-selected exception rather than silently changing queues. Recheck permissions, capacity, image access and storage before creation. Use region `cn-northwest-3`, one GM402 GPU, 20 CPU, 236 GiB, queue-visible access and autosave. No public SSH/EIP or public dashboards.

Reference notebook `rl_dashboard` (`kaic-08ab1432-4d49-4a97-b0d5-f30b00b24113`, SSH `root@36.103.180.181:22`). Reuse its authorized saved image and `/ks3-regular`, `/kpfs-regular` mounts. Saved images do not copy unsaved runtime changes or guarantee `/root/fleet/venv`. Do not save or modify the source implicitly, copy personal files/keys, or overwrite its autosave tag. Install only the user's authorized public key and read the new private address and entitlement live.

Before enrollment, verify cgroup quota, platform GPU entitlement and UUID, prepared Python prefix/packages, writable state/log paths, storage access and outbound control-plane reachability. The source has no Docker socket and no functioning systemd, so begin with the explicit trusted-root process-tree backend. Missing environment or network blocks GPU acceptance; do not silently install dependencies, allocate an EIP, or treat enumeration as a GPU pass.

On 2026-10-30, the created test notebook `kaic-6e552a55-4108-4d14-b719-e8a392e193d9` first failed with registry `NotFound` for the source autosave tag. With user approval, the existing notebook was stopped, changed to the available personal image `rlinf-dashboard:1.1` (`0f71ac0b-6dfe-43ce-9e6f-f2fb94b26874`), and restarted. It is running at private Pod IP `10.0.3.85` in the selected `regular-training` queue with 1 GPU, 20 CPU and 236 GiB, no public SSH/EIP. The reference notebook is used only as an SSH jump; its files and environment were not copied.

A read-only platform recheck on 2026-10-31 found this same notebook stopped, with
successful autosave and a newer personal autosave image. Queue, 1 GPU, 20 CPU,
236 GiB and private-only SSH settings were retained. The previous private IP is
not proof of a live container or a reusable node incarnation. Before another
cloud run, read current platform state and retire/re-enroll a replacement through
the documented exact-identity recovery flow; do not reset the saved environment
to the old base image merely because it was previously approved.

The default Python is 3.11.11 with PyTorch 2.6.0+cu124 and no Ray/RLinf, but further inspection found usable preinstalled environments. No dependencies were installed:

| Python entry point | Observed packages and checks |
| --- | --- |
| `/root/RLinf/.venv-pi05/bin/python` | Ray 2.56.1 and PyTorch 2.11.0+cu126; imports RLinf source from `/root/RLinf/rlinf`, whose `pyproject.toml` declares 0.4.0 (no installed distribution metadata). A CUDA tensor operation returned `42.0`. A real Ray task using one CPU and zero GPUs returned `{"value": 42}`; shutdown succeeded and subsequent observation found no Ray raylet/GCS processes. |
| `/root/RLinf/.venv-lumo2/bin/python` | Ray 2.58.0, PyTorch 2.10.0 and discoverable RLinf source; no installed RLinf distribution metadata. A CUDA tensor operation returned `42.0`. No Ray task was run in this environment. |
| `/opt/rl_dashboard_bundle/env/bin/python` | Ray 2.56.1 and PyTorch 2.7.0+cu128; no RLinf module. A CUDA tensor operation returned `42.0`. |

Both `/root/RLinf` venv entry points resolve to `/opt/conda/bin/python3.11`; preserve and validate the venv prefix when integrating them. The supervisor's canonical-executable policy does not by itself preserve that prefix. The checks above validate imports and small computations, not RLinf training or dependency compatibility for a specific workload. `/root/fleet/venv` remains absent, and the replacement image is not proven to contain later changes from the source machine.

The effective cgroup limits are 20 CPUs (`cpu.max = 2000000 100000`) and 236 GiB (`memory.max = 253403070464`). The cgroup-v2 filesystem is read-only despite the presence of `cgroup.kill`, so the cgroup supervisor backend cannot be used here. There is no Docker socket or functioning systemd. At the initial preflight, the state/work directories had not been installed; their parents were writable. The acceptance deployment below installs private state and work paths. Use the approved trusted-root process-tree backend only after its own preflight and lifecycle tests; it is not a task isolation boundary. `/ks3-regular` has an active read-write `fuse.ks3fs` mount over the platform's local directory, and `/kpfs-regular` is a read-write `kingstorage` NAS mount. Cross-restart checkpoint retention has not been tested.

The pool inventory reports one GM402 allocation while `nvidia-smi` inside the guest names one H100 80GB device; confirm that platform/model mapping before treating the UUID as provisioned entitlement. A root-run opt-in Agent observation test was executed on the notebook through its private jump path: the process-tree supervisor socket, cgroup-v2 limits (`20000m`, `253403070464` bytes) and one GPU UUID were observed successfully; closing the supervisor caused observation to fail closed. This validates local observation boundaries only. At this preflight stage the notebook was not enrolled as an extension Agent. The later single-node acceptance below covers explicit registration and GPU Job dispatch; authorized logs and actual Agent upgrade/rollback remain separate gates. No RLinf training was launched.

Later on 2026-10-30 the notebook was observed stopped, with autosave successful. It was restarted without changing its queue, resource specification or public networking. The private Pod IP remained `10.0.3.85`; the host Node IP changed to `10.0.2.72`, so stale host addresses must not identify the execution. The prepared pi05 Python still reported Ray 2.56.1 and PyTorch 2.11.0+cu126 after restart. Root-run supervisor tests passed for cancellation-before-start, cancellation persistence failure and retry, invalid/cancelled requests, and cancellation recovery across an actual supervisor close/open. The temporary test binary was removed. These tests started no training process, do not prove checkpoint retention and do not enroll the notebook or dispatch a Job.

### Foreground Raw observation Agent (preview)

The standalone API parses its own flags independently of dependency-global flags; `--help` exits successfully without TLS or management credentials. Unknown flags and positional arguments are rejected. Create `rlark-system` (or your configured leadership namespace) before applying RBAC. The namespaced controller Role grants Lease access and Event create/patch for leader election; the API has no Event write permission.

`make -C extensions/container-runtime build` builds a Linux-only `container-runtime-agent` alongside the API/controller and `raw-supervisor`. By default this command reports observations only; the opt-in execution flag enables the scoped Raw process path below. It does not enroll a node, issue certificates, stream logs or install upgrades. Create and validate the native/companion bindings first, install the trusted-root supervisor separately, and provision the dedicated Agent certificate with signed metadata for the exact ContainerNode namespace/name/UID, native Cluster UID, container incarnation, tenant and current `containers.rlark.io/agent-credential` annotation. Never issue native administrator certificates to this Agent. Protect the dedicated CA signing key outside the API/Agent; only the CA certificate is configured on the API listener. These provisioning steps are operator-managed; the administrator tool below automates explicit registration and offline package export. Automatic enrollment/renewal is not implemented.

With operator-provisioned files and a running supervisor, the supported observation command is:

```bash
./extensions/container-runtime/bin/container-runtime-agent \
  --endpoint=https://container-api.example.internal:9444 \
  --tls-cert=/private/rlark/agent.crt \
  --tls-key=/private/rlark/agent.key \
  --server-ca=/private/rlark/server-ca.crt \
  --version=preview-v1 \
  --private-address=10.0.3.85 \
  --supervisor-socket=/var/lib/rlark-supervisor/supervisor.sock
```

Replace the endpoint, identity, version and paths with the actual provisioned values. The endpoint must be an HTTPS origin; redirects are refused. TLS 1.3 and server CA verification are required. The separate API uses `--tls-cert`, `--tls-key`, `--agent-ca`, `--kubeconfig` and `--addr` (default `:9444`); use its restricted RBAC manifest in `extensions/container-runtime/config/rbac/container-runtime-api.yaml`. The API authenticates and authorizes every report, rejects unknown JSON fields or a missing `busy` observation, and takes the heartbeat timestamp from server time. Certificate or credential rotation requires restarting the Agent with newly provisioned files; no automatic certificate reload/renewal is implemented. Revoking the credential annotation fences new reports and allocations without releasing any existing allocation.

The Agent defaults to a successful-report interval of 15 seconds (`--interval`, at most 15 seconds), exponential outage retry up to two minutes (`--retry-max`, at most five minutes) with jitter, and a ten-second local observation deadline. It re-observes before each retry. The local `POST /v1/status` accepts no body and returns occupancy only on the private supervisor Unix socket. Active or uncertain persisted work is busy; a closed/missing/incompatible supervisor prevents reporting. GPU query failures also prevent reporting; there is no idle or empty-GPU fallback. No heartbeat or failed probe proves workload cleanup. The allocator independently rejects reports older than 45 seconds; status repair runs every 30 seconds, so the displayed Ready condition can lag the cutoff by one repair interval.

CPU observation intersects process affinity with every readable ancestor `cpu.max` up to the cgroup-v2 mount; memory uses the smallest finite `memory.max` in that visible hierarchy and omits memory when all limits are unlimited. `--cgroup-mount` defaults to `/sys/fs/cgroup`, resolves the current membership through `/proc/self/cgroup`, and rejects non-v2, missing or malformed boundaries. These are visible upper bounds, not utilization, free capacity, a promise about hidden ancestor limits or task-level enforcement. GPU visibility does not change the provisioned UUID allowlist. Foreground observation is currently Raw-only; the extracted Docker Engine observer is not wired to this command.

Local tests cover failed observations, API outage/recovery without stale replay, cancellation, strict occupancy responses, ancestor quotas, unlimited/invalid limits, duplicate GPU output, mTLS authorization and redirect rejection. On a prepared trusted-root GPU container, the opt-in `RLARK_TEST_LOCAL_OBSERVATION=1` test runs the actual supervisor socket, cgroup and GPU probe without starting a workload. These checks do not complete the production scheduling gate.

On 2026-10-30, the same private notebook passed `TestScopedExecutionRealSupervisor`: real mTLS requests, Host/Coordinator, private supervisor Unix transport, trusted root process start, Agent/Host reconstruction, allocation GPU environment injection and suspension/cleanup proof. The management API was fake and the workload was CPU-only; the injected `GPU-a` was a test sentinel, not an authorized device. The notebook remained `regular-training`, 1 GPU, 20 CPU, 236 GiB, no public IP. Temporary binaries were removed. This is execution-component evidence, not enrolled production Job/GPU acceptance.

### Scoped Raw execution preview

After manually provisioning exact native/companion bindings, a dedicated node certificate and a private supervisor, add `--enable-task-execution=true` to **both** the API and Agent commands. Keep the controller's `--enable-job-reservations=true`. Apply the base API RBAC plus `extensions/container-runtime/config/rbac/container-runtime-api-execution.yaml`; the extra role permits only the native Job/Task reads, Host Task metadata/status patches, companion binding/allocation reads and allocation cleanup status patches. It grants no allocation creation/deletion, Secret, exec, signing or impersonation permissions. The execution route is absent by default; observation remains available without extra RBAC.

The versioned preview `POST /v1/task` channel permits only `assignment`, `get`, `authorize`, `claim`, `status` and `cleanup` for the certificate-bound allocation and Task UID. Strict bounded JSON rejects arbitrary resources and fields. Claim patches are checked in memory against the exact allowed Host metadata and rebuilt server-side with optimistic concurrency. Typed status only writes Host-owned fields/conditions for the current Task incarnation. The Agent does not receive a management kubeconfig. Before start, the API checks the current Job template, restart marker, credential, heartbeat, release, cordon/drain and retained allocation; a changed command or ownership fails closed.

Suspension/deletion is the durable cleanup request. The Agent first calls the private supervisor's idempotent Stop, including stop-before-start, verifies exact identity and durable cancellation plus cleaned descendants, then reports cleanup. The API removes only Host-owned claim metadata before publishing allocation cleanup proof; the Job controller owns lock release and Task allocation protection. A lost claim/cleanup response is recovered from current state. Local observation failure performs cleanup-only reconciliation without synthetic heartbeats or new starts. API outages retain workload and allocation state. Revoked/replaced credentials cannot report cleanup; restore the exact owner's identity or perform the separately audited operator recovery rather than deleting protection.

Tests exercise real mTLS transport with fake management state and supervisor: wrong node/tenant/UID, stale generations/heartbeat/release, forged fields/status, Host restart, GPU injection, committed claim response loss, partial cleanup/lost responses and cleanup after observation failure. These component tests do not establish deployed cloud Job acceptance; the later opt-in real-cluster test below covers explicit registration and a single GPU process. Ray groups, authorized logs, automatic enrollment/renewal, cloud upgrades, UI and full GPU training acceptance remain separate work; runtime-neutral multi-template scheduling is covered by controller tests.

### Implemented execution lifecycle increment

The implemented preview aggregates independently scheduled Raw member Tasks. Running requires current Task `observedGeneration` and `ExecutionKnown=True`; terminal results require current `ExecutionKnown=True` and `ExecutionCleaned=True`. A terminal phase alone, leader exit, heartbeat expiry or missing supervisor record is never release proof. Once a current success/failure result is reported, the scoped API rejects new starts and conflicting status regressions. The Agent must persist supervisor Stop/cancellation before the existing cleanup endpoint publishes allocation cleanup proof. This path must also work without a fresh local observation.

The controller verifies the exact allocation target/UID/owner/tenant, waits for Agent claim cleanup, persists the existing cleanup receipt, and observes allocation deletion before publishing Job success/failure. Pending cleanup is visible separately from the observed Task result. The completed native Task, target and allocation-protection finalizer remain as the current result identity, including when someone requests Task deletion; only Job suspension/deletion, a changed template or an explicit restart marker retires it. Reconciliation after controller/API/Agent restart must not allocate or run that same Task again. A changed execution retires the old Task through the existing cleanup workflow and resets Job timestamps. Timestamps describe control-plane observations, not exact process start/exit times. No CRD or native lifecycle change is required.

Tests cover success/failure and running aggregation, stale/missing conditions, exact cleanup proof, delayed deletion, lost receipt/delete/status responses, controller reconstruction, no rerun/reacquire, Job suspension/deletion/restart after completion, and a second Job acquiring the released node. A real-supervisor test proves terminal cleanup cancels delayed Ensure after release. On 2026-10-30, `TestScopedTerminalExecutionRealSupervisor` passed on the private Kingsoft Cloud notebook with a CPU process, real mTLS/Unix transport and fake management state; it observed Job success and allocation deletion, then rejected a delayed Ensure. This component test alone does not establish enrolled cloud GPU Job acceptance; see the later real-cluster results below. Each member remains an independent process in this preview; logs, automatic enrollment/renewal, cloud rolling upgrades and UI are still separate gates.

### Explicit administrator provisioning (preview)

The independent `container-runtime-admin` binary uses strict JSON configuration and three explicit operations: `cluster-create`, `node-register` and `node-package`. It uses the administrator's management kubeconfig on the provisioning machine; Agents never receive that kubeconfig. Cluster creation creates a new native Cluster, a dedicated namespace and one UID-pinned ContainerCluster. Node registration creates a native Raw Node and its ContainerNode in that selected Cluster, with an explicit container incarnation, private address, prepared environment, network scope and platform-authorized GPU UUIDs. Every create operation has a caller-retained UUID and canonical request digest, so a lost create response is recovered by fresh reads without changing partially created intent; existing resources without that exact operation identity, tenant and matching desired content are conflicts, never adopted. Partial resources stay visible for retry. Registration does not set Ready or release a cordon.

Package export reads an exact ContainerNode UID, revalidates the native and companion Cluster/Node instances and tenant, then issues a bounded-lived client-only certificate from an operator-managed dedicated Agent CA. It never changes or rotates the active credential. The package is written exclusively to a new private directory, with a node certificate/key, API server CA, immutable identity manifest, executable Agent launcher, separate supervisor launcher and optional systemd units. It contains no CA signing key, management kubeconfig or copied environment. Export retries may issue another leaf for the same active credential; a stale/replaced credential fails validation. Operators transfer the package privately and start the supervisor separately. Stopping/updating the Agent must not stop the supervisor. Automatic enrollment/renewal remains later work; idle rollout selection is an opt-in preview described below.

The first package supports the approved Linux trusted-root process-tree Raw mode. It requires explicit canonical executable/work/state/install paths, Linux Agent/supervisor artifacts and SHA-256 verification. It does not infer GPU entitlement or install Python dependencies. Failure tests cover foreign objects, cross-tenant/namespace bindings, replacement UIDs, partial/response-lost creates, invalid CA/expiry, private output permissions and launch argument quoting. Real mTLS tests verify the issued package certificate authenticates the registered node and stops working after credential rotation. This operator path does not establish deployed cloud enrollment. Docker package mode, automatic renewal and systemd installation orchestration remain later work.

Before running the commands, install the extension CRDs, start the independent validation controller/API, and configure a dedicated Agent signing CA plus an HTTPS server certificate whose SAN matches the private API origin. The provisioning identity needs native Cluster/Node and companion list/get/create permissions and namespace get/create permissions. This is a trusted-administrator path; it is not a tenant self-service API. Protect the issuer key as a regular file with no group/other permissions. The requested certificate lifetime is 1–720 hours and cannot exceed the issuer validity. The private API origin must be reachable from the notebook; the Agent never needs a public address.

The checked-in examples under `extensions/container-runtime/config/examples/` are templates only: replace every `REPLACE_...` value after a fresh read from the management API and platform. Run each operation from the provisioning machine with the administrator kubeconfig, retaining the same operation UUID when a response is lost:

```bash
./extensions/container-runtime/bin/container-runtime-admin cluster-create \
  --kubeconfig=/private/admin/kubeconfig \
  --config=extensions/container-runtime/config/examples/provision-cluster.json
./extensions/container-runtime/bin/container-runtime-admin node-register \
  --kubeconfig=/private/admin/kubeconfig \
  --config=extensions/container-runtime/config/examples/provision-node.json
./extensions/container-runtime/bin/container-runtime-admin node-package \
  --kubeconfig=/private/admin/kubeconfig \
  --config=extensions/container-runtime/config/examples/provision-package.json
```

Use `spec.cluster.uid` from the cluster result for `clusterUID`, and `metadata.uid` from the ContainerNode result for `node.uid`; these are different resource instances. Use the active credential annotation for `credentialID`, and a new operation UUID for each new creation intent. A notebook ID or private IP is not proof of the current container incarnation or GPU entitlement. The package example defaults to observation only (`enableExecution: false`); enabling it still requires the API/controller execution flags and supported Job contract above.

The exported directory must be transferred privately to the configured `installDirectory`, preserving its 0700 directory/executable and 0600 data-file modes, verified with `sha256sum -c SHA256SUMS`, and started as two separately supervised processes: `start-supervisor` first, then `start-agent`. The example deliberately uses the prepared `/opt/conda/bin/python3.11` path and does not install Ray, RLinf or CUDA dependencies. The commands create native/companion records and an offline directory, not an archive or remote installation. Create and validate the workload directories on the target before starting; workload, private install and supervisor-state paths must not overlap. Generated systemd units are optional and are not installed automatically; notebooks use the foreground launchers. A partial export retains `INCOMPLETE`, cannot launch, and must be inspected by the operator before exporting to a new directory. Do not remove that marker to bypass an export failure. Registration/package export does not make a node Ready or dispatch a Job. Credentials and desired node configuration are revalidated before and after writing; SHA-256 checks detect damaged files, not a malicious trusted-root workload. Do not restart the supervisor as part of an Agent-only update.

Local validation of this increment: repository `make test-go`, focused provisioning/CLI race tests, independent module `vet`/build, focused golangci-lint (zero findings), and strict bilingual MkDocs build passed. Repository `make lint-go` remains blocked by 30 existing findings in the native Docker/network and related preview code. No cloud enrollment or GPU Job was performed by this provisioning increment.

### Authenticated Agent artifacts (preview)

The independent API and controller provide an opt-in `--artifact-directory`. Both must mount the same operator-managed, read-only artifact tree at a canonical absolute path. AgentRelease Spec remains immutable. `spec.artifactPath` is a canonical relative path inside that tree; absolute paths, traversal, symlinks, non-regular files, unsupported platform/protocol, files over 256 MiB and SHA-256 mismatches are rejected. The controller periodically rechecks the bytes before setting current-generation `Available=True`; missing or damaged files revoke availability without releasing allocations. A controller without a configured artifact store keeps availability false.

The preview `POST /v1/release` route exists only with an artifact store. A verified node certificate selects the current target from its authoritative ContainerCluster; the body cannot name another namespace, tenant, release or filesystem path. Strict JSON accepts `action: manifest` with the Agent OS/architecture, or `action: download` with the previously returned release UID and SHA-256. Manifest responses expose release identity, version, platform, protocol, digest and byte count, never the server filesystem path. No target returns HTTP 204. Download returns only those exact verified bytes. The service rereads node credentials, native/companion bindings and target identity after artifact I/O; rotation, deletion, target replacement, tenant mismatch or incompatible platform fails closed. Expired heartbeat and workload occupancy do not prevent downloading an update; they never authorize installation or release a reservation.

The Agent client uses the existing TLS 1.3 transport, refuses redirects and bounds downloads. It verifies the manifest, byte count, SHA-256 and executable platform again before returning bytes to a future updater. This client boundary does not select upgrade nodes, renew credentials or restart the supervisor. The opt-in node updater described below now verifies and installs the exact binary. The opt-in rollout controller now selects one fresh idle node at a time; external-supervisor recovery remains a separate gate. Trusted administrators publish bytes and AgentRelease records; no unauthenticated file server or upload endpoint is added. Tests cover real mTLS, cross-tenant/credential denial, replaced target/UID, digest corruption, missing/recovered artifacts, path escapes, cancellation and download bounds.

Publication is administrator-managed. Start both components with `--artifact-directory=/private/agent-artifacts` and mount the same directory tree read-only. Copy `extensions/container-runtime/config/examples/agent-release.yaml`, replace its placeholders with the ContainerCluster namespace, tenant, actual version and lowercase SHA-256, and select the matching `amd64` or `arm64` architecture. Place the built Linux `container-runtime-agent` binary at `releases/agent-v1` below that root before creating the AgentRelease. The existing API RBAC allows release reads; controller RBAC allows release reads and status patches. Agents receive no Kubernetes permissions. Never expose the directory through a public static server.

Wait for `status.available: true` with `status.observedGeneration == metadata.generation`, then let an administrator set the selected ContainerCluster's `spec.targetAgentRelease` to `agent-v1`. New releases need new records/paths; do not overwrite immutable bytes. There is no CLI/UI publisher. The opt-in Linux Agent updater uses `TargetRelease` and `DownloadRelease`; administrators select the target release, and the opt-in rollout controller selects idle nodes. Changing the target can fence scheduling until the version is installed and fresh observations confirm readiness. The target alone does not authorize binary installation, stop a running Task or release capacity; installation requires a scoped upgrade operation and allocation. A missing/corrupt artifact produces `Available=False` and a bounded API error; restore the exact bytes or publish a new release and let periodic inspection recover. Do not edit status to bypass verification. Repointing a target is desired configuration, not automatic rollback. Keep the independent supervisor alive throughout any operator-controlled Agent update.

### Upgrade reservations (preview)

Before enabling binary installation, the control plane must serialize upgrades against both workload allocation and other upgrades. An `AgentUpgrade` is one immutable, identifiable operation pinned to the native Cluster, ContainerCluster, ContainerNode, container incarnation, enrollment credential and AgentRelease UID/digest. Its name is deterministic from the native Cluster UID in the authoritative Agent namespace, so only one operation per Cluster can exist. The controller/admin must create that operation explicitly; creating it or marking it Reserved never installs a binary.

Upgrade reservation uses the exact same deterministic `ContainerAllocation` name as Tasks. Its typed `upgrade` member distinguishes upgrade ownership; workload Job/Task fields are absent. Task allocation and upgrade reservation race on one API Create, so at most one can win. Task transport returns no workload assignment for an upgrade-owned lock; neither the task cleanup path nor heartbeat loss can release it. A node must have current validated native/companion membership, a fresh authenticated idle heartbeat, an explicit GPU allowlist, no legacy reservation and an available compatible target. After reservation creation, authorization is rechecked and any partial/lost write is recovered by exact reads.

The reservation records the observed allocation UID on the operation before the Agent may claim `Applying`. The Agent's scoped upgrade protocol must carry the exact operation and allocation UIDs. Success or verified rollback needs a fresh heartbeat for the resulting version and an explicit idle completion report; failures with uncertain installation retain both locks. Target changes, deletion or certificate revocation deny new application grants but never free an in-flight operation. A pending/reserved operation can be cancelled with an optimistic-concurrency status transition before any application grant; cancellation of an Applying operation requires the owning updater to finish or roll back. Completed operations remain visible until their exact allocation has a terminal proof and deletion is observed. Manual operations remain administrator-retained. The opt-in rollout controller can reap its successful operations only after updater journal acknowledgement and exact allocation deletion. The node-local installer below is preview functionality.

The node updater replaces only the Agent process, retains and verifies the previous binary, persists stage/switch/rollback intent, and leaves the independent workload supervisor running. Local `Busy=false` alone never authorizes a switch; the scoped grant and exact persisted attempt are mandatory. Automatic health-timeout rollback after a successful exec is not implemented.

Install all extension CRDs (including `agentupgrades`) and the updated controller RBAC before restarting the controller with `--enable-agent-upgrade-reservations=true`. The flag defaults to false. The controller needs allocation status patch permission to persist cancellation proof; Agents receive no additional Kubernetes or upgrade permissions. After publishing an available target release, fill `config/examples/provision-upgrade.json` with the exact registered node UID/credential, release UID/digest and observed source Agent version. Retain the operation UUID when retrying:

```bash
./extensions/container-runtime/bin/container-runtime-admin upgrade-create \
  --kubeconfig=/private/admin/kubeconfig \
  --config=extensions/container-runtime/config/examples/provision-upgrade.json
kubectl --kubeconfig=/private/admin/kubeconfig -n REPLACE_AGENT_NAMESPACE \
  get agentupgrades.containers.rlark.io -o yaml
```

`upgrade-create` records intent; only the opt-in controller can move it to `Reserved`. Inspect `status.phase`, `status.allocation` and the `Reserved` condition. An unavailable lock stays Pending and retries. Target/credential/identity changes deny new reservations while preserving an already witnessed lock. Cancel a Pending/Reserved operation by deleting its exact observed object; the controller persists cancellation, proves allocation cleanup and waits for observed deletion before removing protection. A stuck or Applying operation requires owning-updater recovery; do not remove finalizers or edit status to bypass it. Automatic operation selection requires the separate rollout flag described below; the scoped protocol and opt-in local installer are described below. Tests cover workload/upgrade competition, lost create/status responses, stale and foreign identities, delayed deletion, deletion protection and denial of Task-owned cleanup.

On the approved private notebook, a fresh check after replacing the image confirmed `/root/RLinf/.venv-pi05` still imports RLinf source, reports Ray 2.56.1/PyTorch 2.11.0+cu126, and executes a CUDA tensor returning `42.0` on one H100. The default conda Python lacks Ray/RLinf; choose the prepared environment explicitly. This is a direct environment check, not an enrolled RLark GPU Job or upgrade acceptance test.

### Delivery gates

#### Scoped upgrade application protocol (preview)

The opt-in `POST /v1/upgrade` endpoint is implemented on the dedicated mTLS API, with `assignment`, `get`, `claim` and `complete` actions. Identity comes exclusively from the verified node certificate. Discovery returns only that node's current operation, its exact allocation UID and path-free release identity. It grants no installation permission. Every other action must echo both operation and allocation references; claim additionally supplies a caller-retained attempt UUID. The API never creates upgrade operations or allocations and grants no general Kubernetes access.

Claim uses an optimistic-concurrency transition from Reserved to Applying, recording the attempt UUID and server start time. It revalidates current native/companion membership, tenant, credential, container incarnation, allocation ownership, fresh idle source-version heartbeat, current available target and artifact integrity. A different attempt cannot take over Applying. A lost committed response is recovered only by the same attempt; changed targets or revoked credentials deny new grants while retaining uncertainty and both locks. Cancellation races with claim through the same operation resourceVersion. GET/discovery never grants or repeats a switch.

Completion accepts only Succeeded at the pinned target version or RolledBack at the recorded source version, with explicit idle proof from the trusted local updater. It requires a current authenticated idle heartbeat after the grant start time and matching result version. The API saves a bounded completion receipt in operation status; it cannot remove allocation finalizers. Completion validates retained ownership separately from target readiness so deletion or target changes cannot prevent the exact still-authorized owner from reporting recovery. Credential revocation still denies reports. Conflicting results and attempts are rejected; failure, timeout, unknown state and heartbeat loss never imply rollback or cleanup.

The controller consumes that exact receipt, publishes allocation cleanup proof and observes deletion before setting the operation terminal. Lost completion, status, delete and finalizer responses converge without another installation. Completed operation identity remains visible until explicit administrator deletion; Applying without a receipt remains protected. The opt-in `--enable-agent-upgrades` API flag and `config/rbac/container-runtime-api-upgrades.yaml` are separate from Task execution. This preview installs only through the opt-in node updater; automatic node selection requires the separate opt-in rollout controller; a UI remains unimplemented. The protocol is covered by real mTLS, wrong node/tenant/UID/attempt, claim/cancel races, lost committed responses, stale/busy/version-mismatched heartbeats, target/credential changes, exact successful/rollback receipts, delayed deletion and controller reconstruction tests.

Enable the API with `--enable-agent-upgrades=true` and apply the additive upgrade API RBAC alongside the base API role. Install the regenerated AgentUpgrade CRD before enabling either component. The Linux Agent polls this endpoint only when its separate `--enable-agent-upgrades=true` flag is enabled. `Client.Upgrade` accepts a strict request of at most 16 KiB and validates the bounded response. `assignment` takes no identity fields; `get` requires both exact references. `claim` also requires the assignment resourceVersion and a canonical nonzero attempt UUID, persisted by the caller before sending. A completed response remains Applying until lock deletion; `get` can omit the now-absent allocation in its response, but the request must retain the recorded reference. HTTP 400 means invalid input, 403 denied identity, 409 competing/stale operation, and 503 unavailable prerequisites. No request may override certificate identity.

#### Agent binary updater (preview implementation)

When `--enable-agent-upgrades=true` is set on the Linux Agent, the serialized heartbeat loop polls `assignment`, claims only a `Reserved` operation, obtains the exact current release manifest and bytes over the authenticated release endpoint, and verifies the immutable UID, version, platform, size, digest and executable format before changing anything. It writes the new executable to a private state-directory staging file, fsyncs it and its directory, and atomically replaces the configured Agent install path while retaining one mode-preserving backup. The supervisor process is never stopped. A small durable receipt records the exact operation/allocation/attempt and expected result before the process replacement; startup reads and validates that receipt, reports a fresh idle heartbeat for the running version, and submits `complete`. A failed `exec` restores the backup and submits `RolledBack` only after the old version is observed idle. The updater never deletes or releases an allocation itself.

The updater uses the same mTLS client and certificate-bound identity as heartbeat. It refuses relative or shared install/state paths, symlinked targets, concurrent attempts, changed release identity, missing allocation, stale source heartbeats, and any response that cannot be proven exact. It does not provide a sandbox or package dependency installation. The foreground launcher and generated systemd unit both run the same Agent process; systemd may restart a failed replacement, while a notebook without systemd relies on the persisted receipt and an operator restart. Automatic rollback after a host power loss is not claimed until an external supervisor restart test is complete.

For new exports, set `enableUpgrades: true` in `provision-package.json`. The generated launcher adds `--agent-install-path=<installDirectory>/container-runtime-agent` and `--upgrade-state-directory=<stateDirectory>-agent-upgrades`. Before launching, create that separate updater directory with mode 0700, owned by root; keep both the install directory and updater directory private and outside workload paths. Existing exports require re-exporting or explicitly supplying the three flags to the Agent. `--version` remains the bootstrap version: after a switch the retained receipt maps the actual `/proc/self/exe` digest to its release version, including service restarts. Do not delete the journal or switch checksums during normal operation. The journal binds endpoint, client certificate and install path; credential rotation, relocation or unknown binary changes require explicit operator recovery rather than silent adoption.

The opt-in package launcher verifies every immutable exported file and accepts the Agent only against its original digest or the updater's private source/target checksum files. New versions still pass ELF/platform/digest validation in the updater before those checksums are written. The backup and staging files each retain at most one binary; interrupted temporary files can be removed only while the Agent is stopped and no operation is applying. Claim and completion response loss reuse the durable attempt; digest/identity errors, filesystem failure and uncertain state retain the allocation and retry. Only a failed process replacement restores the previous executable automatically. A new binary that starts but cannot heartbeat does not have an automatic timeout rollback; keep the backup and journal and recover with an administrator. Run systemd/foreground and cloud upgrade acceptance before treating this preview as unattended rollout support.

- [x] Paired design documents record the independent extension, selected queue and acceptance scope.
- [x] Independent module/build and extraction of providers/supervision/tests; opt-in native integration seam and ownership validation.
- [x] Typed companion schemas, scoped mTLS authorization and administrator provisioning for the single-node preview; direct companion writes remain administrator-only. Generic Job ownership dispatch is implemented.
- [x] Explicit container Cluster onboarding, scoped Agent identity and whole-GPU allocation, accepted on the private notebook.
- [x] Bounded authorized Raw remote logs, accepted against a real private-cloud GPU Job.
- [ ] General admission/migration and automatic enrollment/renewal.
- [x] Authenticated release hosting and target-driven idle rollout selection are implemented as preview infrastructure; private-cloud single-node rollout and launcher-restart acceptance passed.
- [ ] External-supervisor power-loss recovery and failed-exec rollback acceptance.
- [x] Independent Container Clusters UI, opt-in Raw Job submission and bilingual API/operations examples; cloud browser submission/retry/suspend acceptance passed. The controller now expands up to 16 independent Raw templates into stable Tasks; cross-member Ray/RLinf coordination remains open. Ray/RLinf multi-node submissions remain incomplete; independent Raw templates are supported by the reservation controller.
- [x] Authorized private 1-GPU notebook and single-process Raw GPU Job success/reuse/failure/stop acceptance.
- [x] Private-cloud Agent upgrade and post-upgrade GPU lifecycle evidence.
- [x] Checkpoint retention through Job success/failure/stop and resource deletion in the same running container; cross-replacement storage persistence remains unverified.
- [x] Recovery/race tests, extension Go tests/vet/build and strict bilingual docs build.
- [ ] Full repository Go/Web checks and generated verification.

Test competing allocations, stale UID/generation/incarnation, cross-tenant access, duplicate enrollment, partial effects, restarts, supervisor loss, failed cleanup, heartbeat loss, digest mismatch, upgrade/allocation races, rollback failure, migration conflicts, checkpoint retention and unchanged Kubernetes behavior. Do not mark a gate from scaffolding or mocks.

### Automatic idle rollout selection (preview)

The extension implements a separate opt-in controller, `--enable-agent-rollouts=true`, which requires `--enable-agent-upgrade-reservations=true`. Install the regenerated ContainerCluster and AgentUpgrade CRDs and additive `config/rbac/container-runtime-controller-rollouts.yaml` before enabling it. The rollout controller uses an uncached management client; it owns only the `AgentRolloutReady` condition and `status.lastAgentUpgrade`. The existing Cluster validator continues to own binding validation and phase. `spec.targetAgentRelease` remains administrator-owned desired configuration. Clearing it pauses new selection; it does not undo an Applying operation or remove its lock.

This preview serializes operations, with omitted/zero `maxUnavailable` interpreted as one simultaneous upgrade; only 0 and 1 are supported. This is an upgrade concurrency limit, not a guarantee that all other nodes are schedulable: the existing release fence can exclude nodes still on the old version. It does not preempt a running Job. Select nodes by stable name and UID, requiring exact tenant/native/companion bindings, authorized GPUs, current-generation Ready/Validated conditions, a fresh authenticated idle heartbeat the opt-in `agent-upgrade-v1` capability and a matching `agent-platform/<os>/<architecture>` capability. Ignore `status.schedulable` for upgrade selection because the target-version fence intentionally makes old versions unschedulable. A workload or upgrade allocation excludes its node, address and GPUs; final reservation still races atomically against workloads on their shared node lock. Busy, stale, offline and non-upgradable nodes remain pending. Version equality is the existing observation contract, not remote binary attestation; use distinct release versions for different bytes.

Create an immutable, exact-UID AgentUpgrade in the existing exclusive per-Cluster slot, with a deterministic request UUID and explicit rollout-owner annotation written in the initial Create. Repeated reconciliation and lost Create responses recover the existing operation. Do not adopt, cancel or reap manual/foreign operations. Target changes automatically delete only this controller's unassigned operations with no allocation witness or attempt, using UID/resourceVersion preconditions. Once a reservation has been exposed, preserve the operation for explicit updater/operator cancellation and recovery: a pre-claim journal or lost response may already exist. Applying operations are never automatically cancelled. Applying, failed, rolled-back or uncertain operations block further selection and retain their locks/proofs; an administrator investigates and explicitly removes a recovered failed/rolled-back operation before retrying.

After a successful authenticated completion, wait for the upgrade controller's current-generation `AllocationReleased` proof and the owning updater's `acknowledge` action. The updater fsyncs its terminal journal before acknowledging the exact operation/allocation/attempt/result/version; the API records `status.acknowledgedAttemptID`. Lost completion responses keep the operation available for recovery; lost acknowledgement responses are retried from the saved terminal journal, even if the allocation has already disappeared. Only an authenticated current credential with a fresh idle result heartbeat can acknowledge. Older Agents without this handshake are not automatically reaped. Upgrade the API before enabling the new updater and rollout controller. Save one bounded `status.lastAgentUpgrade` receipt pinning operation, node, release, digest and completion time before requesting deletion with UID/resourceVersion preconditions. Observe deletion before selecting the next node. The upgrade controller can finalize this already-proven terminal operation without touching a later workload allocation at the same node key. A finalizer delay, status conflict, response loss or controller restart cannot skip this order. Only the latest successful receipt is retained; export operation history externally if needed. A fresh target-version observation from every current member is required for `AgentRolloutReady=True`; an empty cluster is waiting, not complete. Private-cloud single-node rollout acceptance passed; multi-node rollout, external-supervisor power-loss recovery, failed-exec rollback and health-timeout rollback remain unsupported. UI integration remains incomplete.

On 2026-10-31 the existing private notebook was restarted from its current autosaved image without changing `regular-training`, 1 GPU, 20 CPU, 236 GiB or public-network settings. Private SSH confirmed one H100, PyTorch 2.11.0+cu126, Ray 2.56.1 and CUDA tensor result `42.0`. `TestCloudRawObservation` passed against real cgroup/GPU/Unix-supervisor boundaries: `cpu=20000m`, `memory=253403070464`, one visible GPU and idle status. The test also verified that upgrade capabilities are absent by default and include protocol/platform only when the updater is enabled. Docker socket and systemd remain unavailable. This observation run alone established neither enrolled cloud GPU Job nor actual Agent rollout acceptance; the later lifecycle and rollout results below cover the single-node GPU path.

### Real single-node Raw acceptance (opt-in)

On 2026-10-31, the private H100 notebook was explicitly registered in the separate `ksyun-containers` Cluster (`ksyun-container-agents` namespace, tenant `team-a`) and started the exported root supervisor/Agent package. Fresh mTLS heartbeats reported Ready and schedulable, 20 CPUs, 236 GiB and one authorized GPU. Against the real KCP management API, `TestCloudRawLifecycle` passed all four cases in 170 seconds: CUDA success (`42.0`), a second Job reusing the same GPU, exit-code-7 failure, and stopping a running process. Each case observed allocation absence, then deleted its own Job/binding and verified Task absence. The node returned idle/schedulable and no allocations remained. Deployment acceptance also exposed and fixed an API `--kubeconfig` registration panic, missing leader-election Event RBAC, and missing controller logger initialization. Test-only API reachability uses an SSH reverse tunnel and short-lived credentials. This establishes single-node, single-process preview acceptance; Ray/RLinf groups, historical logs/stats, native UI integration and real failed-exec rollback remain incomplete. Checkpoint retention through task cleanup, bounded remote logs and idle Agent rollout have separate acceptance evidence below. Native Cluster/Node aggregate UI still uses its own Lease-based status; the companion Ready observation is not a native Node Lease and must not be presented as completion of that UI integration.

Update the deployed native Job/Task/Node CRDs **and** native controller-manager to the current repository version before enabling the independent extension controller. Old Job schemas reject `placement.environment`; an old native controller can continue applying its legacy Raw policy to extension-owned Jobs and write conflicting conditions. The current generic `rlark.io/controller-owner` boundary is required. Do not remove ownership annotations or disable validation to work around a version mismatch. The extension controller initializes RLark's structured logger so reconciliation and dependency errors remain visible.

Prepare the dedicated Agent CA, server TLS certificate and the restricted API/controller service accounts. Keep signing keys on the provisioning machine. Create/register/export with the administrator CLI; transfer only the node package. Use a reachable HTTPS endpoint; for an isolated test a loopback-only SSH reverse tunnel can carry the mTLS connection. This tunnel is a test dependency and does not establish a durable production ingress. Agent credentials and the local test service-account tokens expire independently; this test used 24-hour credentials, without automatic renewal.

The real process executable must exist at a canonical, non-symlink path on the node. In the pi05 acceptance environment the venv's `python` symlink resolves to Conda; blindly resolving it loses the venv prefix. A new regular copy of the same Python executable inside that venv's `bin` directory was checked to preserve `sys.prefix`, PyTorch imports and CUDA, then allowlisted as `/root/RLinf/.venv-pi05/bin/python-rlark`. This is a verified test-environment adaptation, not an automatic Python installer or a portable venv migration strategy. Re-export a package to a new private directory when changing paths; do not edit exported scripts/configuration or bypass their checksums. Create the actual workload directory before starting the supervisor.

Run the following only against an enrolled, idle, dedicated one-node test Cluster with one authorized GPU. Both API and Agent execution flags and controller reservations must be enabled. Replace every environment value with that deployment's authoritative resource and path. The kubeconfig stays on the test runner; it is never sent to the Agent.

```bash
cd extensions/container-runtime
RLARK_TEST_RAW_KUBECONFIG=/private/management.kubeconfig \
RLARK_TEST_RAW_NAMESPACE=ksyun-container-agents \
RLARK_TEST_RAW_CLUSTER=ksyun-containers \
RLARK_TEST_RAW_NODE=ksyun-test-1 \
RLARK_TEST_RAW_TENANT=team-a \
RLARK_TEST_RAW_EXECUTABLE=/root/RLinf/.venv-pi05/bin/python-rlark \
RLARK_TEST_RAW_DIRECTORY=/root/rlark-smoke \
GOWORK=off go test -v ./pkg/integration -run '^TestCloudRawLifecycle$' -count=1 -timeout=15m
```

The test is skipped unless the kubeconfig variable is present; all other inputs then become required. It validates the exact Raw Cluster/Node/tenant binding, current idle heartbeat and absence of allocations before creating workloads. Cases execute real CUDA tensor computation, reuse the GPU under a second Job identity, exit with code 7, and suspend a long-running GPU process. Each case waits for the current Job result and observed allocation absence before exact-UID/resourceVersion deletion of its Job and binding. It also waits for the owned Task to disappear. On failure it retains remaining objects for inspection and dispatches no next case. If its Job remains, inspect that exact instance and suspend it through normal reconciliation if required; never force-delete uncertain allocations/finalizers. Supervisor receipts/logs remain for local administrator inspection. Optional remote-log assertions are documented below; garbage collection remains unsupported.

Completed-operation deletion also handles the KCP deletion-time generation increment. Only an immutable operation with a complete authenticated receipt and the controller’s durable exact-allocation-release condition at its original observed generation can finalize across that single increment. Invalid attempts, missing release proof and unexpected generation changes retain protection. The controller does not rewrite status to make a stale proof appear current and does not inspect or mutate a later workload allocation.

### Bounded log acceptance (validated)

The test runner needs a management kubeconfig authorized to create/delete native Jobs and companion bindings; controller/API ServiceAccounts deliberately lack those administrator permissions. For optional remote-log checks, enable API task execution and its loopback administrator listener, export the node with `enableExecution: true` and `enableLogReads: true`, and supply these additional variables alongside the lifecycle command above:

```bash
export RLARK_TEST_RAW_LOG_ORIGIN=http://127.0.0.1:9446
export RLARK_TEST_RAW_LOG_TOKEN_FILE=/private/gateway-admin-token
```

The origin must be an explicit loopback HTTP address: use the API read listener or the console configured with `--log-api-origin`. The token file must be a private regular file containing a short-lived Gateway-issued administrator token, never the issuer key. Keep it off the node and do not record it in command arguments or logs. Invalid inputs fail before work is created. The stop case waits up to one minute for actual CUDA/output readiness after observing Running, checks unauthenticated 401, unknown `path` 400 and wrong Task UID 409, then verifies exactly 65,536 bytes with the GPU marker and truncation. Stop uses normal suspension; release must finish before the old identity returns 409. An empty snapshot is valid before output arrives (`data: ""`, the canonical base64 string).

On 2026-10-31 all four lifecycle cases and direct log checks passed in 170 seconds; the console-proxy stop/log/cleanup case passed separately in 28 seconds. The dedicated node returned idle/schedulable, with no acceptance Jobs/Tasks, bindings or allocations. The first proxy run exposed an acceptance race (Running precedes Python/CUDA stdout); the exact retained Job was normally suspended and cleaned before retry. These checks establish bounded on-demand logs only, not historical archives, follow/tail, stats, terminal/exec or production ingress.

### Checkpoint-retention acceptance (validated)

Set `RLARK_TEST_RAW_CHECKPOINTS=true` alongside the lifecycle and remote-log
inputs to test retained workload files. This mode requires authenticated logs:
the stop case must prove it wrote its checkpoint before suspension, rather than
mistaking process creation for application readiness. Each case exclusively
creates a uniquely named JSON checkpoint in the configured work directory and
fsyncs it. A subsequent Job verifies the exact content of every earlier file
after the earlier Job, Task, binding and allocation have disappeared. A final
verification Job checks all four checkpoints, including the stopped process's
file. Missing or altered files fail the run; verification never recreates them.
Only the final successful verifier explicitly removes these test-owned files.
Failure retains files and resources for inspection. This verifies lifecycle
cleanup inside the running container, not storage survival across platform
replacement, checkpoints on arbitrary mounts, or framework checkpoint integrity.

On 2026-10-31 the five cases passed on the private H100 notebook in 216 seconds: success, GPU reuse, exit-code-7 failure, normal suspension, and final checkpoint verification. All acceptance Jobs/Tasks, bindings, allocations and test files were absent afterward; the node was idle and schedulable. An earlier run timed out because the Agent polled one API process while log reads reached another; its exact Job was normally suspended and deleted after confirmed allocation/Task cleanup, and its four test files were verified before explicit removal. The successful run used one API process for both Agent polling and the log listener. The cloud platform subsequently reported the notebook stopped at 04:20 with image autosave successful. A repeat after adding an explicit checkpoint-ready log-marker assertion was rejected by the stale-heartbeat preflight before creating any work; that additional assertion has not been rerun on this notebook. Re-read instance identity and explicitly retire/re-enroll if platform startup replaces the container; old Ready evidence does not authorize dispatch.

### Cloud idle-rollout acceptance procedure (validated)

This acceptance gate passed on 2026-10-31 against the private Kingsoft Cloud H100 notebook. The procedure and evidence below are retained for repeatable operator validation. Re-read the notebook state, queue, resource specification and private network before starting an existing stopped notebook. A platform stop/restart can replace the container, PID namespace, host and GPU. Compare current incarnation evidence with the registered `instanceID`; never reuse old credentials or update immutable companion Spec to adopt a replacement. After proving the old instance stopped and all its Jobs/Tasks, allocations and upgrade operations absent, explicitly retire its exact native/companion Node instances and register the replacement with new UIDs, incarnation evidence and credentials. Preserve the separate Cluster. Retained supervisor records belong to the old incarnation and must not be used to authorize new executions.

For a dedicated idle one-node test Cluster, export a new package to a new private install/state directory with `enableExecution: true` and `enableUpgrades: true`. Keep administrator credentials and CA signing keys on the provisioning machine. Verify the package checksums and prepared Python/CUDA environment; start the independent supervisor, create the separate root-owned 0700 updater directory and start the Agent. Do not modify an old exported launcher to enable upgrades. Confirm fresh `agent-upgrade-v1` and `agent-platform/linux/amd64` capabilities before selecting a target.

Stage a separately built Linux Agent binary as an immutable release with a digest different from the running Agent. A packaging-only rebuild validates switching/transport; it does not prove compatibility between different source versions. Configure the same read-only artifact tree on the API and controller. Apply the additional API-upgrade and controller-rollout RBAC, enable the API upgrade route before the controller's upgrade reservations and rollout selector, and wait for the release's current-generation availability. Retain workload reservations while restarting control-plane processes; a component outage never proves cleanup.

The opt-in `TestCloudRawRollout` uses the same `RLARK_TEST_RAW_KUBECONFIG`, `RLARK_TEST_RAW_NAMESPACE`, `RLARK_TEST_RAW_CLUSTER`, `RLARK_TEST_RAW_NODE` and `RLARK_TEST_RAW_TENANT` inputs as the GPU lifecycle test, plus `RLARK_TEST_RAW_RELEASE`. The release must already be staged and available. The test required exactly one fresh idle upgrade-capable member, no allocations/upgrade operations, and no configured target. It selected the target with optimistic concurrency, observed an exact node/release/digest completion receipt, current-generation rollout readiness, a fresh schedulable target-version heartbeat, and deletion of the upgrade/allocation objects. It left the target configured during verification; the operator then cleared the target to return the dedicated cluster to its paused state. Do not run it against a shared or production Cluster.

```bash
cd extensions/container-runtime
# Set the existing RLARK_TEST_RAW_* identity/kubeconfig inputs first.
RLARK_TEST_RAW_RELEASE=agent-cloud-accept-v2 \
GOWORK=off go test -v ./pkg/integration -run '^TestCloudRawRollout$' -count=1 -timeout=10m
```

The first rollout changed the Agent from PID 282/start ticks 996548774 and digest `c0a8f4…` to the target digest `db5714…`; supervisor PID 246 and digest remained unchanged. Restarting the idle Agent through the checksum-valid launcher preserved the installed `cloud-accept-v2` journal result rather than the bootstrap version. A second rollout using immutable `cloud-accept-v3` bytes changed the same Agent PID 497 from `db5714…` to `606fd3…` while retaining supervisor PID 246; `TestCloudRawRollout` passed in 32 seconds after the first rollout exposed the KCP deletion-generation issue and the fixed controller recovered its retained finalizer. The post-upgrade GPU lifecycle rerun passed success, reuse, exit-code-7 failure and suspension in 166 seconds. This evidence does not establish multi-node advancement, failed-exec rollback on this cloud image, host power-loss recovery, renewal or health-timeout rollback. A failed/uncertain installation keeps its allocation and journal; never force-delete them to make the test pass.

## Historical preview evidence

The remaining sections of this document retain the prior in-tree Raw/Docker experiments as migration evidence. They are not the target extension API, and their historical multi-node scope does not replace the single-node acceptance gate above.

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
- [x] Typed API, schema/deepcopy generation, compatibility and ownership tests for the preview contract.
- [x] Durable exclusive reservation, rollback and recovery for one Task per Node.
- [x] Single-node supervised Raw execution through Host/Coordinator and the polling Agent in the historical native preview.
- [ ] Multi-node Ray barriers and group lifecycle.
- [x] Node registration, scoped credentials, GPU authorization and environment validation for the preview path.
- [ ] rlarkadm package, authorized logs, Gateway and UI integration.
- [ ] Examples, runtime capability/deployment/operations documentation in both languages.
- [ ] Focused and repository Go/web/doc checks; draft PR with evidence.

## Acceptance and failure tests

Cover competing reservations, partial rollback, controller restart, crash after external effect, Agent reattach, PID reuse, supervisor loss, orphan descendants, busy ports, unknown GPU/environment, wrong node/tenant claims, head/worker failure, network loss, stop versus failure, retained checkpoints and Kubernetes compatibility. CPU fake/process tests run locally. Real two-container Ray/GPU and minimal RLinf tests require prepared environments and explicit authorized GPU scope; do not silently install dependencies or launch cloud training. Record unverified behavior explicitly and keep maturity preview until end-to-end evidence exists.

## Current implementation evidence and remaining integration

The historical native preview wires an experimental Raw Agent polling loop, scoped heartbeat, restricted API, Job dispatch and Host/Coordinator path for one trusted root process Task on one pre-provisioned Node. It is not production Ray/RLinf or multi-node Job support. The independent companion path described above now has a separate reservation controller and an opt-in scoped single-process execution/cleanup channel. Do not submit Ray/RLinf training until authenticated dispatch, group barriers, port allocation and GPU/platform acceptance are complete. The generated `status.raw`, `status.reservation`, `spec.placement.binding` and `spec.placement.environment` fields remain historical preview contracts and are not used by the new companion reservation path.

Implemented boundaries:

- `rawallocation.Manager` uses fresh reads and optimistic status patches for whole-node reservations, incremental commit, same-execution recovery and rollback of uncommitted claims. The environment is frozen in the reservation. Validation checks membership, node/container identity, heartbeat, cordon/drain, tenant, private network scope, distinct training IPs and non-overlapping GPU UUIDs across the RLark inventory. This does not prove platform entitlement exclusivity or protect against processes outside RLark.
- `AssignTask` assigns an already committed reservation to one Task UID, idempotently. Allocation commit alone never authorizes an execution. Every node must have an assigned Task UID before node admission accepts the complete group. Admission also freshly verifies that every peer Task exists, is unsuspended, has its exact binding, and belongs to the same authoritative Job instance.
- `raw.NodeAdmission` re-reads the Task, owning Job and all reservations. It rejects direct Tasks, stale generations, suspension, replacement containers, wrong tenant and incomplete groups. The restricted server API permits only scoped reads, heartbeat, status and claim writes; discovery, watches, tunnels and arbitrary resources are denied.
- `raw.Provider` implements the existing Runtime Provider and inventory contracts. It delegates starts and cleanup to an independent supervisor; unconfirmed cleanup remains in progress. Raw Host claimant identities require the exact node UID and never adopt legacy claims.
- `raw.NewCoordinator` and `raw.NewHost` assemble explicit workload adapters, versioned process-carrier decoding, node-scoped claims, execution and status through the existing framework. Process manifests contain typed API metadata whose namespace/name/UID must match the Task; validation and lowering have no process effects. Host restart, duplicate reconciliation, sibling-node isolation, suspension cleanup, Job replacement and ownership conflicts are tested. The Agent polls every five seconds to repair lost notifications. No built-in Ray adapter is registered. Unknown observations project to `Pending` with `ExecutionKnown=False`; this does not prove the process has stopped.
- The Linux supervisor persists intent before starting, supports explicit cgroup and trusted-root process-tree backends, records results, and only accepts Task identity on a private Unix socket. Agent client reconnection is tested. A supervisor restart marks uncertain persisted work Unknown and blocks reuse; it does not adopt PIDs.

### Experimental supervisor prerequisites and build

Build with `make -C apps/rlark raw-supervisor`. This target is opt-in and does not imply Raw Agent support. The default `cgroup` backend requires Linux with cgroup v2, `cgroup.kill` and kernel support for atomic `CLONE_INTO_CGROUP` placement. The operator must provide a writable delegated subtree and a distinct nonroot workload UID/GID. Workload processes must not be able to migrate to other cgroups, write cgroup control files, read node credentials, or write supervisor state/socket paths. An ordinary cloud container may lack these permissions; test delegation first, without giving the container a Docker socket or assuming that privileged mode is acceptable.

The strict JSON configuration passed to `bin/raw-supervisor --config /private/supervisor.json` uses these fields:

```json
{
  "backend": "cgroup",
  "stateDirectory": "/var/lib/rlark-supervisor",
  "executables": ["/usr/bin/python3.12"],
  "workDirectories": ["/workspace/training"],
  "logBytes": 10485760,
  "cgroupDirectory": "/sys/fs/cgroup/rlark-workloads",
  "workloadUID": 10001,
  "workloadGID": 10001
}
```

Paths must exist and be canonical absolute paths, including resolving executable symlinks before configuration. The state directory is private (0700), outside workload paths, and retained across Agent restarts. No Agent/supervisor environment is inherited by training. Authorized executable files and their parent directories must remain protected from workload replacement; path allowlisting alone does not establish file integrity. The cgroup backend accepts an explicit environment and runs the configured executable as the workload UID/GID. The process-tree backend runs trusted workloads as root; private filesystem modes are not a security boundary against those workloads. The Unix socket is 0600; only the trusted local Agent identity may use it. Authentication/authorization for remote logs is not supplied by this socket.

Current logs retain only the first `logBytes` bytes per execution and discard excess while draining process output; rotation, tail, since, follow and Gateway routing are not implemented. Records and per-execution cgroup directories are not automatically garbage-collected. Do not remove active or uncertain records to bypass ownership checks. A supervisor stop/restart while work is active can leave uncertain descendants; use container/platform lifecycle evidence and an audited recovery procedure before provisioning a fresh node incarnation. The current code deliberately has no force-release/recover endpoint. Checkpoints outside private supervisor state are not deleted by this supervisor.

### Independent-process admission versus distributed readiness

Independent `raw.rlark.io/process` members do not wait for all sibling allocations
before starting. A Job with two templates and one eligible node can complete one
member, retain its terminal Task identity, release its exact cleaned allocation,
and then run the waiting member on that same node. A missing or already released
sibling allocation must not become a generic Agent start fence. Current Job,
Task, credential and own-allocation checks still apply to every start; the durable
`TaskSetRetiring` condition still blocks starts during whole-set replacement.

This is intentionally distinct from the planned Ray/RLinf adapter. That adapter
must reserve and verify every required member before training, with exact run
membership, port claims, bounded readiness deadlines and confirmed group cleanup.
Independent-process partial admission is not evidence of distributed readiness.

Local regression combines the actual reservation controller, mTLS Task API and
Host/Coordinator with a fake supervisor. It covers two templates sharing one node,
first-member success or failure, unavailable-supervisor cleanup, controller
reconstruction, rejection of a late old-member cleanup request, and aggregation
only after both allocations are cleaned. This is local integration evidence,
not cloud GPU or distributed-training acceptance.

### Ordered integration work

1. Add measured CPU/memory capacity from effective cgroup or affinity limits, resource observations and operator-facing health.
2. [x] Add runtime-neutral multi-template validation and legacy conflict handling. Expand a Raw Job into stable per-template Tasks with independent allocations and aggregate cleanup. Ray/RLinf barriers remain a later step.
3. Define and validate the typed Ray/RLinf workload contract: exact run/member identity, supported versions, head/worker roles, network scope, private addresses and port requirements. Reject this workload at dispatch until the complete adapter is wired.
4. Assemble the adapter with whole-group reservation, preflight/ready barriers, bounded deadlines, per-network-scope port reservations, exact membership and one training-result owner. No training before every assigned member is verified. Distinguish Unknown from failed, and stop all assigned members on confirmed group failure.
5. Complete cleanup/retry ownership: retain reservations until every supervisor confirms cleanup, including Job deletion/restart and partially dispatched groups. Add auditable operator recovery for supervisor loss; absence of a heartbeat never releases capacity.
6. Add the foreground enrollment/start package, authorized log route and bounded retention, UI capability gates and per-replica progress. Add executable EN/ZH deployment and training examples only when this route works.
7. Run two prepared containers with the explicitly chosen supervision backend, authorized GPU UUIDs and supported Ray/RLinf versions. Verify busy ports, descendant cleanup, Agent restart, supervisor loss, network loss, head/worker failure and retained checkpoints. These platform tests have not been run locally.

### Local validation evidence

The internal packages pass focused race tests and the repository `make test-go` suite. The supervisor binary builds, generated API/client/UI models have been regenerated, and the paired documentation passes a strict MkDocs build. The historical preflight below predates the real single-node GPU acceptance; the current acceptance result is recorded above. No delegated-cgroup or two-container Ray/RLinf acceptance run has been completed. Repository Go lint still reports baseline native findings; the changed extension packages have no new findings under the incremental check. This evidence and the current single-node result do not complete the multi-node delivery checklist above.

### Kingsoft Cloud container preflight (2026-10-30)

The previously identified development container was inspected over its existing SSH connection. Only short CPU processes and capability probes were run; no GPU computation, dependency installation, platform task changes, parent cgroup writes or filesystem remounts were performed.

| Probe | Observed result | Consequence |
| --- | --- | --- |
| Kernel and cgroup | Linux 5.15; cgroup v2 mounted read-only | Creating a child cgroup fails with `EROFS`; the cgroup backend cannot run on this container. Presence of `cgroup.kill` alone is insufficient. |
| CPU and memory boundary | CPU affinity: 192; `cpu.max`: `12000000 100000`; `memory.max`: `1523639648256` | Affinity alone overstates effective CPU capacity; the quota is 120 CPU equivalents. |
| GPU visibility | `nvidia-smi` lists six H100 80GB devices | Device enumeration works, but visibility does not establish allocation authorization. No GPU execution was attempted. |
| Default Python | `ray`, `torch` and `rlinf` distributions absent from the inspected interpreter; no `ray` on PATH | This interpreter is not a prepared training environment. Follow-up inspection found prepared environments; see below. |
| CPU subprocess | A short Python child exits successfully | Ordinary process execution works. |
| User/PID namespace | `unshare --user --map-root-user --pid --fork` succeeds | A possible alternative isolation boundary exists; it is not implemented by this supervisor. |
| Namespace descendant cleanup | Namespace PID 1 exits; its child creates a new session; parent verifies no surviving non-zombie processes by PID/start time | The kernel cleaned up the tested CPU descendants despite `setsid`. This is a feasibility probe, not Ray/GPU acceptance or a production ownership/recovery implementation. |
| Private proc mount | Combined user/mount/PID namespace with `--mount-proc` fails on filesystem propagation with `EPERM` | Do not assume mounting a private `/proc` is permitted. A namespace implementation needs a separate design and validation. |

The available notebook-list response did not identify this SSH endpoint, so its platform GPU entitlement remains unverified. Do not infer an authorized GPU allowlist from the six visible UUIDs. The current cgroup supervisor is blocked on this target. Continue production integration only after an execution boundary compatible with the actual container is implemented and tested; do not silently fall back to process-group-only cleanup, privileged mode, or host cgroup remounting.

### Trusted-root process-tree backend

The approved cloud-container mode runs the Agent, service, dedicated execution worker and training as root, retaining the container's original PID namespace and `/proc`. Select it explicitly; cgroup preflight failure never activates it automatically. It requires Linux subreapers, pidfd operations, a matching `/proc`, and a trusted workload. There is no extra container, mount namespace, UID separation or task-level CPU/memory hard limit; the outer cloud container remains the resource boundary. One container is occupied by one Task until cleanup is proven. Root training can modify service files or credentials: this is not a sandbox or a mutually untrusted multi-tenant execution boundary.

Build `raw-supervisor`, resolve its installed path and Python symlinks, create the working directory and pass this configuration (replace paths with actual canonical paths):

```json
{
  "backend": "process-tree",
  "workerExecutable": "/usr/local/bin/raw-supervisor",
  "stateDirectory": "/var/lib/rlark-supervisor",
  "executables": ["/usr/local/bin/python3.11"],
  "workDirectories": ["/workspace/training"],
  "logBytes": 10485760
}
```

`workloadUID` and `workloadGID` must be omitted or zero; `cgroupDirectory` must be omitted. Unknown backends or mixed credentials/cgroup settings are rejected. The worker executable is the same installed `raw-supervisor` binary; its `--process-tree-worker` mode is internal and requires inherited protocol descriptors, not a user workload CLI. Restarting the Agent reconnects to the existing service socket; do not restart the service as part of an Agent restart.

For each execution, the service persists intent and launches one dedicated worker. That worker enables subreaping before starting training, accepts an explicit environment and retains the original `/proc`. Only kernel-proven child relationships authorize signals. During cleanup it opens pidfds, verifies child ownership using `waitid(P_PIDFD)` before signalling and repeatedly terminates/reaps adopted descendants. It confirms completion only on `ECHILD`, not an empty process-name/PID scan. It does not kill by root UID, command name, port, process group, global `ray stop`, or `pkill`. Normal main-process exit still triggers descendant cleanup. A stop request gives the main process 30 seconds after SIGTERM, then uses SIGKILL; main termination and descendant cleanup each have a 30-second confirmation bound. Failure to confirm either blocks reuse.

A successful worker outcome and normal worker exit must both be observed before the service persists `cleaned=true`. A missing/malformed outcome, worker death, interrupted intent or uncertain persistence keeps the execution occupied. Restarted services never adopt old process IDs, even if the worker might still exist. Recover uncertain executions through verified platform container replacement; there is no force-release endpoint. Host boot ID and Agent restart alone do not prove container replacement. Checkpoints outside service state remain untouched. Agent loss does not instruct the worker to kill training, but loss of the service itself is not transparently recoverable.

Follow-up cloud probes located Python 3.11.16 environments: `/root/fleet/venv/bin/python` has Ray 2.59.0, PyTorch 2.10.0 and RLinf 0.4.0; `/opt/rl_dashboard_bundle/env/bin/python` has Ray 2.56.1 and PyTorch 2.7.0. User/PID namespaces can run CPU Ray tasks but yield psutil PID errors against the inherited `/proc`; even `include_dashboard=False` starts some background services. Private proc and tmpfs mounts also fail at temporary mount points. The selected process-tree backend avoids this mismatch. GPU visibility is confirmed, GPU entitlement and real GPU/multi-container training remain unverified. Resolve Python executable symlinks before authorizing any environment.

The root process tests cover detached descendants, unrelated-process preservation, duplicate ensure/stop, completed-result recovery and missing-outcome recovery. These tests do not complete production Agent registration, scoped authorization, Job replica dispatch, Ray barriers, authorized logs or UI integration. Until those are wired and end-to-end tested, the delivery checklist remains incomplete.

Actual cloud-container acceptance: the newly built supervisor ran the process-tree backend as root and started a Ray 2.59.0 CPU task (1 CPU, 0 GPUs) through its private socket. The task returned `{value:45, uid:0}` and finished `Succeeded` with `cleaned=true`. Duplicate Ensure reused the execution; every Observe used a new connection. Stop returned a deletion tombstone only after confirmed cleanup. No dependencies were installed and no GPU computation was run. This does not validate the complete RLark Job/Agent route.

Only canonical executable paths are currently accepted. Resolving a Python venv symlink can lose the venv `sys.prefix` and package search path; the probe explicitly supplied `PYTHONPATH` and `PATH` for the prepared environment. It does not prove that passing the venv symlink directly works. Production environment adaptation must still validate the Python prefix and dependency resolution.

Additional failure tests cover socket reconnection during execution, escalation after the full 30-second SIGTERM grace period, and result persistence failure. A failed result write returns Unknown with `cleaned=false`; repeated Stop after storage recovery cannot release the node. Restart reads the original durable intent and remains blocked.

## Docker Agent validation

The Docker preview path has been exercised with a root Agent using the host Docker socket and Docker Engine API 1.44+. The validation covers Agent startup, Node capacity/incarnation observation, durable allocation checks, container ownership, and retryable cleanup. A real multi-host GPU Ray training run, Gateway log/stats routes, systemd package generation, and provider-specific device UUID allocation remain outside the completed contract.

The single-host Ray smoke Job `docker-validation-20261030` exited successfully (exit code 0), with `RLARK_DOCKER_SMOKE_OK` in its logs. After Job deletion, its Task, Docker container, and Node reservation were confirmed absent. Each Docker host currently accepts only one RLark Docker Task, including CPU-only Tasks.

## Cloud Raw lifecycle rerun on 2026-10-31

After the notebook restart from its current autosave image, derived from the approved `rlinf-dashboard:1.1` replacement, the current container incarnation was registered with a new ContainerNode UID, credential and PID-namespace/start-time identity. The package for commit `4b8258c` was copied into a new private install/state directory. A temporary SSH reverse tunnel was re-established because the notebook has no public IP; this is an operator test path, not production ingress. The notebook remains in `regular-training` with one H100/authorized GPU UUID, 20 CPU, 236 GiB, and no Docker socket. The saved environment was preserved without resetting the base image or installing dependencies.

`TestCloudRawLifecycle` passed in 165.10 seconds with the prepared `/root/RLinf/.venv-pi05/bin/python-rlark` environment (Python 3.11.11, PyTorch 2.11.0+cu126, Ray 2.56.1, RLinf source):

1. CUDA tensor computation returned `42.0` and the Job succeeded.
2. A distinct Job reused the released GPU and succeeded.
3. A process exiting with code 7 produced a Failed Job and cleaned allocation.
4. A running process was suspended, stopped and cleaned.

The final node heartbeat was fresh with `Ready=True`, `Schedulable=True`, capacity `20000m` CPU / `253403070464` bytes memory / one GPU, and no remaining allocation, Task, ContainerJob or AgentUpgrade. This proves the current single-node, single-process Raw route after restart; it does not prove Docker Engine access, multi-node Ray/RLinf barriers, checkpoint persistence or production ingress.

## Completed increment: console node registration

The independent console now provides opt-in administrator Raw node registration,
reusing provisioning operations and exact native/companion UID ownership. The
certificate/package export remains on the administrator machine. API/UI tests
cover lost/partial responses, foreign identities, validation, disabled capability
and authorization. Cloud enrollment and package installation remain separate
operational gates; see the [console contract](container-runtime-console.md#administrator-node-registration).

## Current increment: independent Raw template expansion

The reservation controller accepts one to sixteen uniquely named independent
`raw.rlark.io/process` v1 templates per externally owned Job. Each becomes a
stable native Task (`<job>-<template>`) with one exact allocation. A container
Node runs at most one member at a time. Members may select different prepared
environments or Node selectors. Empty or duplicate names, `Head`, downstream
edges, SSH, Pod templates, legacy bindings and GPU visibility overrides are
rejected by the shared controller/Agent contract.

This is partial admission: available members can run while others await capacity,
and a member can reuse a node only after its previous member's allocation cleanup
and deletion are confirmed. It is suitable for independent processes. It provides
no simultaneous-start guarantee, Ray head/worker barrier, port reservation,
RLinf/NCCL membership, or automatic sibling cancellation after a failure. Do not
use it to launch distributed training with interdependent members.

Job `Pending` covers materialization, capacity waits, and observation/cleanup waits
when no member is running. `Running` means at least one member is currently running.
`status.tasks` exposes each member's current result; stale observations appear
Pending. Terminal Job status waits for every member's current Succeeded/Failed
result, known/cleaned conditions, exact cleanup proof, and observed allocation
deletion: any failure yields `Failed`, otherwise `Succeeded`. A failed member does
not stop independent siblings. An unknown member retains its lock; the Job cannot
become terminal. Completed Task identities remain protected against replay.

`Scheduled=True` means every unfinished member was successfully reserved on that
reconcile; completed/released members need no new allocation. `ExecutionObserved`
means at least one member has a current observation. `AllocationReleased=True`
requires the whole set to finish cleanup. Suspension/deletion requests stop on
every member before waiting for individual cleanup. A changed template, restart
marker or allocated Job generation retires the whole old set. The controller
persists `TaskSetRetiring=True` before retirement and observes the old set absent
before allowing any replacement, including after restart or partial deletion. This recovery latch is honored across Job generations and also fences Agent start authorization.

Upgrade the extension API before the controller; old APIs still reject multiple
templates. There is no native schema change, and single-template submissions stay
compatible. The console submission API/UI remains single-process; administrators
can test multiple independent templates through the management API:

1. Copy `extensions/container-runtime/config/examples/raw-processes-job.yaml`.
   Replace the tenant, prepared environment, canonical Python executable and
   authorized work directory. Create the suspended Job with `kubectl create -f`.
2. Read its UID and the existing native Cluster UID from the authoritative API.
   Replace those UIDs, tenant, Cluster name and agent namespace in
   `extensions/container-runtime/config/examples/raw-processes-binding.yaml`.
   Create the binding with `kubectl create -f`; do not reuse another Job's UID.
3. Activate with `kubectl patch jobs.rlinf.io independent-raw-processes
   --type=merge -p '{"spec":{"suspended":false}}'`. Observe Job, Tasks and
   `containerallocations.containers.rlark.io` in the agent namespace.
4. To stop, patch the Job's `spec.suspended` to true. Wait for `Stopped`, exact
   allocation absence and Task absence before deleting the binding and Job. An
   uncertain cleanup retains identities for recovery; never force-remove locks.

Controller tests cover separate node locks, partial capacity, cleanup behind a
blocked earlier member, stale results, all-member terminal aggregation, lost
child-create responses, controller reconstruction, whole-set replacement, and
terminal identity retention. Agent tests authorize the exact non-first template
and reject malformed, missing or ambiguous members. These are component tests;
no multi-node cloud GPU or Ray/RLinf acceptance is claimed for this increment.
