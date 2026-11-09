# Notebook Fleet delivery plan

Status: implementation in progress. This is the delivery checklist, not a claim
of supported end-to-end Fleet operation. Each acceptance result must identify
the tested deployment and distinguish automated tests from real infrastructure.

## Review and revised delivery gates

The 2026-11-04 review takes commit `6e36db8` as an implementation checkpoint,
not an acceptance or release. The earlier order below remains the target; the
following gates add a baseline review and move the robot network feasibility
check ahead of full orchestration. Do not treat historical IPs, credentials,
observations or passing tests as evidence of the current deployment.

| Area | Current evidence and gap | Next gate |
| --- | --- | --- |
| Baseline | Enrollment, relay, mixed Job and UI changes are intertwined; automated tests exist but do not prove a deployed system. | Review ownership, API compatibility, generated schemas and failure paths; preserve unrelated changes and record reproducible test results. |
| Enrollment | A has a historical authenticated heartbeat; B's replacement did not complete. The controller pins operations, identities and releases, but actual recovery and replacement still need validation. | Verify A/B join, retries, replacement, cancellation, credential handling and restart against current instances. C is a later expansion test. |
| Transport | A process-scoped single-port Ray/RLinf PoC passed across two hosts; no robot path or Job-owned automated injection has passed. | First verify robot-to-A outbound reachability and compatible Linux/glibc, Ray, Python, PyTorch and Gloo settings. Then test synthetic traffic, actors and bidirectional Channel with private credentials, scoped participants, disconnection and cleanup. |
| Mixed Job | Controller preview pins Kubernetes targets and tracks cleanup; it does not create the head, coordinate member startup, or provide Fleet readiness barriers. | Design Job-owned head placement and run identity, staged startup, timeouts, critical-member failure and cleanup before implementing and testing these behaviors. |
| UI | The current console exposes node enrollment and Raw Jobs, not the desired mixed Fleet workflow. The large page mixes data, state and presentation, while the enrollment form is dense. | Review existing UI visually and agree on a design for hierarchy, primary actions, progress, errors, responsive layout and accessibility. Split data/form/presentation without changing API or authorization semantics; test real flows and both languages. |
| Acceptance | Cloud model prediction and the relay PoC were separate tests; neither proves a real robot Passive run. | Run the full acceptance in item 7 below, record evidence, and explicitly report skipped or blocked checks. |

Proceed in this order: (1) baseline and design review, (2) A/B enrollment and
replacement, (3) robot transport feasibility and managed transport,
(4) mixed Job orchestration, (5) agreed UI redesign and integrated submission,
and (6) end-to-end acceptance. UI design can be reviewed during earlier gates,
but its progress must not imply that backend features are ready. Each gate
requires focused failure/recovery tests and synchronized English and Chinese
documentation. No cloud or robot action may touch independently owned work.

The first acceptance targets B for the head and rollout, as confirmed on
2026-11-04 and shown in the diagram below. Scheduling must still respect Job
placement and resources rather than hard-coding a node name. Earlier topology
illustrations placed the head on A and must not override this acceptance choice.

### Proposed console direction

Replace the dense all-in-one page with a cluster overview and a selected-cluster
workspace. The overview prioritizes health, capacity and the next actionable
step; the workspace separates nodes and enrollment, connectivity, Jobs and
maintenance. A guided enrollment flow shows topology and release selection,
followed by per-node installation, Agent and transport status. A mixed Job flow
shows role placement and resources before submission, then distinct startup,
workload and cleanup progress with bounded logs and actionable errors. Unknown,
stale and unavailable are never shown as healthy. Destructive actions use
explicit confirmation and remain subject to server-side authorization.

Before implementation, review desktop and narrow-screen states, light and dark
themes, keyboard access, long identifiers and both languages against the
existing RLark visual system. Use realistic loading, empty, partial-failure and
recovery states. Visual review complements, but never replaces, API and runtime
tests; no feature should appear enabled before its backend contract is ready.

## Accepted architecture

A/B/C are existing Notebook containers in one logical RLark container Cluster.
They run the Raw runtime without a Docker socket, systemd, TUN or a second
Kubernetes installation. A is a gateway and compute node; B/C are workers.
RLark connects to A over SSH for installation, and A connects to workers using
its own preconfigured SSH identity. Private keys never pass through the UI.

The robot x86 remains in its own Kubernetes Cluster. One native Job and its
extension binding own the cloud and robot Tasks. RLark schedules the only Ray
head alongside B's rollout in one Raw execution; the Env worker joins that same
Ray cluster. Stopping the Job cleans that head and its descendants. Existing
Jobs and independently owned heads must not be adopted or stopped.

```text
RLark -- TLS management :30000 --> A -- private network --> B/C
                                  ^                       B: head + rollout
                                  |
                          TLS multiplexing :30001
                                  |
                           robot Env Pod --> S1U
```

Inference payloads bypass the local RLark service. SSH is installation/control
only. No HTTP proxy, SSH tunnel, WireGuard or global routing change is required.
A exposes management 30000 and data 30001; 30002 is unused. Internal Ray, Gloo
and Fleet Driver TCP connections share the data gateway. The first version
admits one active cross-cluster Job per gateway. Checkpoints and model settings
are workload configuration, not RLark cluster or model-catalog configuration.
GPU activity/keepalive and Notebook power management are excluded.

## Delivery order and acceptance

1. **Baseline:** remove GPU keepalive API/UI/package/service behavior; fix the
   legacy UI response test; regenerate schemas; add enrollment, installer and
   service-manager failure/recovery tests. Keep ordinary GPU discovery and
   allocation. Go and UI checks must pass.
2. **Enrollment:** one UI flow creates the Cluster, persists topology and pins
   a release, installs A, establishes management communication, then installs
   workers via A. UI and CLI share retained operation IDs and authoritative
   resources. SSH, Agent, services and data readiness remain separate. Partial
   worker failure preserves successful work. Test real A/B enrollment and
   addition of C when available.
3. **Maintenance:** explicit UID-bound replacement/retirement checks all Job,
   allocation and upgrade references before revoking credentials. Unknown
   cleanup blocks reuse. A restarted Notebook (new instance/IP) rejoins by
   resubmitting enrollment for its name; the controller replaces the idle
   stale record and waits (`ReplacementBlocked`) while work still holds it. Node
   services survive manager restarts; complete bundles are verified. Upgrade
   workers only while idle; A is busy while forwarding an active Job. Credentials
   renew using their exact identity; disruptive transport changes wait for idle.
4. **Managed transport:** Job-owned network sessions pin participants, Task and
   execution identities, peer addresses and internal ports. Distribute credentials
   privately; automatically inject the Linux/glibc TCP shim into the Raw process
   tree and the robot Pod only. The robot uses a transport sidecar sharing its
   network namespace. Configure IPv4/Gloo devices explicitly. Disconnection
   invalidates affected streams: reconnect never replays bytes or promises
   transparent Ray recovery. Verify actual robot outbound access to A:30001,
   then synthetic traffic, Ray actors/objects and bidirectional RLinf Channel.
5. **Mixed Job:** reserve B, resolve the head, create Env, provision transport,
   start the head, join workers, verify membership and Channel, then start Fleet.
   Publish separate placement, transport, head, membership, Channel, workload and
   cleanup conditions. Kubernetes keeps resource scheduling authority for Env.
   This is not atomic cross-runtime gang scheduling; timeout cleans partial
   starts. Critical member failure stops siblings. Explicit retry waits for old
   cleanup; no unlimited whole-Job retries or global process killing.
6. **UI:** a dedicated mixed/Fleet submission form selects cloud/robot targets
   and submits each role's resources, command, environment and configuration.
   Model fields remain opaque workload input. Display the single head,
   participants, startup progress, bounded logs and stop/cleanup state. API/CLI
   submissions appear in the same views; a full remote Fleet Dashboard is not
   required for this delivery.
7. **Real acceptance:** re-read current Notebook identities/IPs and robot state;
   do not assume historical addresses are live. Verify B CPU/CUDA Job lifecycle,
   model loading, one shared Ray cluster ID, RLinf Channel and real S1U Passive
   inference. Require at least 10 minutes and 20 complete observation/prediction
   round trips, recording load time, latency, failures and dropped frames. Stop,
   prove owned resource cleanup, retain checkpoints, and submit again.

The workload acceptance uses
`/ks3-regular/ss/checkpoints/test/lumo2_1022_exp1_pack_anything_pack_the_suitcase_20261023142019`.
Fleet validates the processor, weights, normalization and interface. Passive
does not enable Active or send execution actions. Existing robot Jobs remain
untouched; wait if required resources cannot safely coexist.

## Implementation and compatibility

Keep runtime/enrollment/transport/controllers in the independent
`extensions/container-runtime` modules and UI components in the main RLark UI.
Core changes are limited to generic, tested adapters, including target Cluster
UID validation before native Kubernetes Agents consume extension-owned Tasks.
Persist desired topology separately from one-shot start/replace operations and
observed receipts. Enforce ownership, tenant, UID and generation checks at
every entry point, including direct management API writes.

Extend existing typed Cluster/Node/Job companions and release contracts rather
than exposing arbitrary remote commands. Generate CRDs/deepcopy from source.
Preserve existing single-process Raw submission, agent-only releases and older
Gateway reads. New behavior remains preview until the corresponding acceptance
gate passes. Document blocked infrastructure separately from implementation.

## Validation and progress

### 2026-11-04 connection and upgrade investigation

At approximately 19:56 China time, A's SSH on public port `10001` was
authenticated against the previously trusted host key. A second SSH listener
is managed by its Notebook Supervisor; the original port `22` was retained.
Container replacement may remove this local service configuration. New A/B
enrollment operations for this deployment must use A's port `10001` and pin
the existing A ContainerNode UID; do not edit or retry an older operation with
different immutable targets.

B was reached through A using the configured strict SSH host trust. Its current
SSH endpoint was `10.0.3.85:22` and its observed container address was
`10.1.2.88`, rather than the earlier `10.1.2.82`; one H100 was visible. A
Supervisor's `rlark-b-gpu-keepalive` service ran a `python-rlark` process with
working directory `/opt/rlark-b-keepalive` and about 708 MiB of GPU memory in
use. At approximately 20:13 China time, after operator approval and an identity
check, that specific service was stopped; the subsequent GPU process query
reported no compute processes. Its Supervisor configuration was not removed or
disabled, so it may restart when the Notebook or Supervisor restarts. These are
time-bound observations, not proof of Agent registration, exclusive ownership,
or readiness. Probe the current authenticated instance and addresses again
before a new operation.

A's enrolled Agent and gateway processes were running, but an existing Agent
upgrade remained Applying and A's heartbeat repeatedly became stale. An
authenticated download through the deployed management path returned HTTP 200
but was interrupted after approximately 29.5 MB of a 42.9 MB artifact; the
running API binary predates the source's longer artifact write deadline and
gzip support. The old Agent's synchronous upgrade attempt delays further
heartbeats. Current source includes asynchronous transfer, but the pinned target
binary and its installation have not yet been validated on this deployment.
The separate platform-managed gateway service was also observed failed against an obsolete private address;
it is not the currently listening enrolled gateway.

With operator approval, the management API was restarted at approximately 20:13
using the candidate containing the extended transfer deadline and gzip support;
the previous running and on-disk binaries were backed up separately. The service
became active and listened on its configured port. At 20:15 the existing Agent
still timed out reading the artifact and the upgrade remained Applying; the
heartbeat at 20:15:08 does not establish sustained health. The independent Ray
process retained its PID and start time through this observation. A separate
authenticated gzip download probe received HTTP 200 but timed out after 15
seconds with about 11.8 MB transferred; it did not complete the artifact.
This is evidence of a transfer constraint, not a successful upgrade. The target
binary has not been installed. A later comparison of the pinned ELF with a
current-source build found that the pinned binary lacks the asynchronous
updater download function present in the new build. The current build is not
the pinned release. A subsequent operator-authorized replacement preparation
staged the new binary on A, registered it as
`agent-current-replacement-20261104`, and observed the release as available
after digest and platform validation.

At approximately 20:33–20:36, an operator-authorized emergency handoff paused
the Cluster target, verified the A instance, the old attempt and allocation
UIDs, and replaced only the enrolled Agent. An initial process-readiness race
triggered rollback; a subsequent handoff verified the running executable and
reported multiple fresh heartbeats for `preview-current-replacement-20261104`.
The gateway, Raw supervisor and independent Ray process retained their original
process identities. The old Applying operation and its exact allocation were
deleted with UID and resource-version preconditions; no successful upgrade was
fabricated. The Cluster now targets the new release, and A's Agent Ready and
AgentReleaseReady conditions were observed true while A remained cordoned.
The four older AgentRelease objects in this Agent namespace and the two
identified old artifacts and temporary old-Agent backups were removed. The
terminal historical enrollment records remain for audit and reference a
removed release; do not retry them. No old-Agent binary rollback is retained
for this handoff. Other workloads were not adopted or stopped.

This was a manual recovery, not validation of the normal updater protocol or a
completed artifact transfer. The handoff modified A's launcher and package
checksums in place, so it no longer matches the original immutable enrollment
package; a later reinstall must use a new verified package and a fresh,
identity-checked enrollment rather than replay the old package. The running
controller was not started with `--enable-agent-rollouts`, and its
AgentRolloutReady condition was stale; selecting a target does not prove that
automatic rollouts are active. A controller also logged optimistic-lock
conflicts while updating A's native Node; investigate before treating its
resource reconciliation as healthy. Revalidate B's identity and ownership
before a new enrollment. At 20:38, strict SSH through A reached the previously
observed B hostname and private address `10.1.2.88`, but this is not Agent
registration or a workload acceptance. The current enrollment controller
installs the selected release on every target, including an existing Gateway;
its service identity includes the release digest. A new-release package for A
therefore has a different identity from A's retained service manager, which
must not be silently adopted or restarted. A controller change now verifies
and reuses an explicitly UID-pinned, healthy existing Gateway without an
installation; deployment and real A/B acceptance remain separate gates. On
2026-11-04 B stopped at 20:41 and was running again from 20:43. Its private
address changed to `10.1.2.91`, and the separately installed GPU keepalive
restarted. Revalidate the instance and process ownership immediately before
any further action. These observations do not constitute A/B, automated
upgrade, transport, or workload acceptance.

Run focused failure/recovery tests during implementation, then extension Go
tests/vet, affected native Go tests, UI tests/build, repository lint/tests,
documentation build and example/schema validation. Real cloud tests require
explicit test inputs; a skipped cloud test is not an acceptance pass.

The 2026-11-04 baseline review reran both extension Go suites and vet, affected
native Task and Gateway packages, 158 UI tests, the UI build and the bilingual
strict documentation build. Go tests required `TMPDIR=/tmp` in this environment:
the default temporary root made Unix socket paths too long. These results do not
validate a deployed cluster, network or robot.

The subsequent local controller change preserves a succeeded enrollment result
when a later probe reports replacement, address drift or malformed output;
those observations remain visible per node and do not trigger installation.
Automated coverage includes matching-probe recovery and historical release
deletion. The change is not yet a real A/B enrollment or replacement acceptance.
At a read-only cloud check on 2026-11-04, B was stopped, then appeared running
after a new platform start at 19:38 CST. Its platform-reported Pod IP was
`10.0.3.85`; this is not proof of its container-private address, identity or
Agent readiness. Re-read all of those before enrollment. Do not start a stopped
Notebook automatically.
At 19:40 CST, the management API reported the container Cluster and A Offline,
A cordoned, and the robot Cluster Online. A's previously configured SSH address
accepted TCP but closed the SSH handshake; this does not authenticate the host.
The existing A/B enrollment was Cancelled, a later operation had failed with
`NodeIdentityConflict`, and no B ContainerNode was observed. Do not reuse stale
probe results or infer that the running B is enrolled.

Verified on 2026-11-04: both extension Go module suites, the affected native Task
packages and all 155 UI tests pass; the UI production build passes. Enrollment
has dedicated SSH outage/recovery, retained operation, identity conflict,
installer and service-manager restart tests. New packages fence Notebook
replacement before starting/adopting services. The restricted local test user
cannot inspect `/proc/1/ns/pid`; deterministic fixture tests cover namespace,
PID 1 start-time and boot-ID changes. Automatic transport injection and full
cloud-to-robot Passive acceptance remain outstanding.

### Deployment checkpoint: 2026-11-04

The management API authentication recovered after its restart. The updated
Notebook CRDs, enrollment controller and enrollment-enabled native Gateway were
deployed. A retained operation installed A through strict SSH, discovered its
private address, started the management gateway and Raw supervisor, and received
authenticated Agent heartbeats with `preview-enrollment-20261104`. Native Node
`ksyun-a` and Cluster `ksyun-containers` were observed Online. A remains cordoned
because independently owned Fleet/Ray work is already running there; it was
neither adopted nor stopped. This proves A's management enrollment, not Ray data
readiness, a cloud workload launch, upgrade acceptance or continuous availability.

B's platform SSH endpoint `10.0.3.85` was reachable through A and resolved to
container address `10.1.2.82`, with one visible H100. The platform reported B
stopped at 18:26 CST before installation completed. B has no new registered
Node or Agent heartbeat. The retained A/B operation remains Waiting with B
`SSHUnavailable`; it must not be reported as a successfully joined cluster.
Do not restart a stopped Notebook automatically. Re-read both addresses and
the container identity after the operator restarts it; a changed immutable
target requires a new operation. Preserve existing owned-node UIDs when adding
or starting other participants.

Real installation exposed and fixed differing A/B Python layouts, missing
private updater directories and a stale API server leaf certificate. The
profile now supports verified address placeholders and per-node canonical path
allowlists; the local manager validates paths before starting services. Apply
matching CRDs **before** starting the updated controller: an old schema may
prune fields such as node role. A schema-pruned registration must be retired
through normal ownership/cleanup checks and re-created explicitly; do not patch
immutable identity fields or remove finalizers to bypass the conflict.

At this checkpoint the native Go suite, both extension Go suites, extension
vet, all 155 UI tests, UI production build and bilingual documentation build
passed. No B rollout, cross-cloud robot Channel, ten-minute Passive run or
stop/resubmit acceptance is claimed. Existing Job `jo-a12e2db6627847af` remains
outside this operation's ownership.

### B enrollment and bounded inference on 2026-11-04

After B restarted, a new enrollment pinned A's existing Gateway UID and used
its SSH port `10001`. Operation `enroll-2c08e0f3-323a-4c7f-aafa-74ddb6a4e131`
succeeded with authenticated A/B heartbeats. B's then-current private address
was `10.1.2.91`; its ContainerNode UID was
`65431a65-e6b2-4a12-a8a0-bd07285a63b4` and native Node UID was
`1e26cf68-7ba9-4a4f-802d-9146441e4eed`. These are instance-specific evidence,
not reusable identities after a Notebook replacement. A stayed cordoned and its
independent workloads were not stopped or adopted. The separately owned B GPU
keepalive was stopped with authorization; it can restart on Notebook restart,
and the platform may stop an idle Notebook.

A single-process synthetic Lumo2 Job was submitted through the administrator
Gateway. Its first execution used the `pi05` environment and failed because its
Transformers installation lacked `qwen3_5`. The Task reported failure and exact
allocation cleanup; the failed Job and binding were retained. The subsequent Job
`raw-e06df611-691d-4619-85c4-7a9fc9443afe` (UID
`99bd3d3c-edd7-47e2-8baf-d242d836325f`) used the pre-existing Lumo2 site
packages with the authorized interpreter and a 600-second process limit. Its
Task ran on B, returned a finite `32 x 29` result in 166.82 seconds, and reached
Succeeded with execution cleanup and allocation release confirmed. This is a
bounded synthetic test with no robot connection or action execution, not the
mixed Fleet workflow or an acceptance of automatic rollout or robot transport.

### B restart request on 2026-11-05

At 09:54 China time, the operator requested a start of the existing stopped B
Notebook. The cloud accepted the request, but subsequently reported it Pending;
its scheduler event reported an unschedulable Pod group. This event does not
identify the underlying capacity or configuration cause. At the time of this
observation no new Fleet Job had been submitted. After B becomes Running, verify
its new container instance, address, GPU ownership, Agent registration and
checkpoint before attempting dispatch. Do not treat the retained historical
node UID or the previous successful synthetic Job as current readiness.

### Robot relay path on 2026-11-06

A's management gateway now also serves the Ray TCP relay on 30001 from the
same process: the enrollment profile's `gatewayArgv` passes
`--ray-config=/opt/rlark-ray-gateway/ray-gateway.json`, and A was re-enrolled
with only its RLark services stopped. Independent Fleet/Ray and training work on
A kept running. A new 30-day relay CA, gateway and peer certificates were
issued; peers are pinned by leaf certificate.

The robot x86 reaches the cloud through the robot Orin, which bridges to the
local router. The x86 holds two addresses on `enp2s0`: `192.168.0.10/24` on the
robot LAN and `192.168.31.250/32` on the router network. Only the
`192.168.31.0/24` route names `src 192.168.31.250`; the default route does not,
so the kernel sources Internet traffic from `192.168.0.10`, which the router
does not translate. Public TCP and the configured public DNS servers therefore
time out from the x86, while the same destinations succeed when the socket is
bound to `192.168.31.250`. The development host reaches A because it sits
directly on the router network.

The relay peer avoids a system route change by binding only its gateway
connection with `localAddress: 192.168.31.250`; with that setting the robot
peer established its session to `36.103.180.181:30001`. A host-wide fix is to
add `src 192.168.31.250` to the x86 default route
(`ip route replace default via 192.168.0.11 dev enp2s0 src 192.168.31.250`)
and persist it in the host network configuration. That change was **not**
applied; it affects every process on the robot, including DNS.

Robot deployment notes:

- The robot Env image `fleet-runtime-cpu:4090-20261027-numpy2.2.6` uses
  Ubuntu 20.04 with glibc 2.31. The connect shim must be built against that
  glibc (the build host's 2.35 output requires `GLIBC_2.34`), and the relay
  binary is built with `CGO_ENABLED=0`.
- `/run` is mounted `noexec`; keep the relay binary under an exec-capable
  host directory such as `/var/lib/rlark-ray-transport`.
- Projected Secret files are symlinks, which the relay rejects for its private
  key; copy them to private regular files before starting the peer.
- The robot's RLark Agent image predates `headAddress` and target-Cluster UID
  enforcement. The Fleet Env container reads `RAY_ADDRESS` itself, so the
  mixed Job sets it in the Pod template; the native fence is not exercised
  until that Agent is upgraded.

Both B and the robot peer hold sessions to A:30001. No Ray process, Fleet
service, mixed Job or robot action has run over this path yet.

### Mixed Job on the Pod network, 2026-11-06

On the evening of 2026-11-06 (Beijing time), the robot Agent was upgraded to
`20261106-agent-67eaa39`. Mixed Job `fleet-mixed-b-s1u-20261106-2` was then
submitted in shared mode: suspended Job, `ContainerJob` binding, and
activation with UID and resourceVersion preconditions. The Raw rollout runs
on B (private address `10.1.2.196` after a restart). The Kubernetes Env runs
on `s1u-01-x86`. This is an operator-assembled preview; the relay is not yet
injected by RLark.

- **Env on the Pod network.** With `hostNetwork`, the Env's fixed Ray ports
  `41001`/`41002` collided with an independent Ray on the robot host. The Env
  now uses the Pod network with a fixed Calico address `10.242.164.100`
  (`cni.projectcalico.org/ipAddrs`). The gateway peer table needs an explicit
  IP, so the address must not change. The relay peer is a sidecar in the same
  network namespace. Host Ray was not touched.
- **Egress without changing routes.** Calico NAT follows the default route,
  which has no `src`, so Pod traffic to A:30001 times out for the same reason
  host traffic did. The fix is a `hostNetwork` Deployment, `rlk-relay-forward`:
  - it listens only on `192.168.0.10:31030`;
  - it forwards raw TCP to A:30001 from `192.168.31.250`;
  - mTLS and certificate pins remain end to end between the peer and A.

  The robot's default route is still unchanged.
- **Driver topology.** The Fleet Driver reads the RLinf topology once at
  bootstrap. The B launcher therefore waits until every Ray member is alive
  before it starts the Driver. RLinf requires `RLINF_NODE_RANK` on every node
  or on none, so the head sets rank `0` because the Env sets rank `1`.
  Without these two changes, the Driver reported one node and bootstrap
  failed.
- **GPU sharing.** The Notebook's `gpu_keepalive` keeps running. The rollout
  config sets `allow_shared_accelerators: true`. When Fleet starts, the
  keepalive drops from about 40 GiB to its small hold.
- **Node selection.** Neither enrollment nor the native Node sets the
  `rlark.io/node-name` label. Pinning a Raw Task to a Notebook therefore needs
  that label added to the native Node by hand, guarded by UID and
  resourceVersion.

Observed: the Driver reports `num_nodes: 2`, with rank 0 `10.1.2.196` and
rank 1 `10.242.164.100`. The rollout loaded its weights next to the
keepalive. The robot moves only when an operator starts a rollout from the
Fleet UI. This is not yet the 10-minute Passive acceptance in item 7.

See [console](container-runtime-console.md), [gateway](container-gateway-plan.md)
and [mixed Job preview](mixed-container-job-preview.md) for current contracts.
