# Notebook Fleet delivery plan

Status: implementation in progress. This is the delivery checklist, not a claim
of supported end-to-end Fleet operation. Each acceptance result must identify
the tested deployment and distinguish automated tests from real infrastructure.

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
   cleanup blocks reuse. New instance/IP is never silently adopted. Node
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

Run focused failure/recovery tests during implementation, then extension Go
tests/vet, affected native Go tests, UI tests/build, repository lint/tests,
documentation build and example/schema validation. Real cloud tests require
explicit test inputs; a skipped cloud test is not an acceptance pass.

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

See [console](container-runtime-console.md), [gateway](container-gateway-plan.md)
and [mixed Job preview](mixed-container-job-preview.md) for current contracts.
