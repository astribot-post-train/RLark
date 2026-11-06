# Container gateway and managed enrollment plan

Status: partial implementation. The independent extension has a tested,
optional A/Hub mTLS relay and a completed acceptance on the authorized Kingsoft
Cloud A notebook described below. UI enrollment/retirement workflows, automatic
certificate renewal, Docker Engine execution, and the cloud-to-robot data path
remain incomplete.

The controller-level [mixed Job preview](mixed-container-job-preview.md) now
places Raw rollout and explicitly bound Kubernetes Env Tasks under one native
Job, while keeping A/B/C in one logical container Cluster and the robot in its
existing Kubernetes Cluster. Runtime-specific completion and stop recovery have
automated tests. Fleet startup barriers, automatic sibling cancellation, relay
injection and Lumo2/S1U acceptance remain outstanding; this is not a completed
cloud-to-robot rollout path.

## 2026-11-02 A enrollment acceptance

The retained `ksyun-containers` cluster now has ContainerNode `ksyun-a` bound to
its native Raw Node. A connects to the RLark Hub at the public TCP 30000 endpoint
with the dedicated pinned mTLS gateway; no SSH forwarding is used at runtime. The
authenticated ContainerNode heartbeat reports the authorized five H100 GPUs,
100 CPU and the observed memory capacity. Restarting only the local Hub caused a
clean disconnect and a fresh heartbeat, confirming reconnect behavior.

A is an existing notebook container without Docker Engine, systemd, or
`CAP_NET_ADMIN`, so this acceptance uses Raw process execution and does not create
Docker containers or provide a Kubernetes network. Three additive supervisord
programs manage the gateway, Raw supervisor, and Agent without restarting the
notebook's existing services. The native Node remains
`spec.unschedulable=true` because existing Ray/Fleet work consumes the machine;
no test Job was dispatched. The Agent package currently uses a 72-hour
node certificate and must be manually regenerated and installed before expiry.
For this installation the node certificate expires at 2026-11-05 19:41:27
Asia/Shanghai (11:41:27 UTC); automatic renewal is disabled. Agent log-read and
rolling-upgrade endpoints are also disabled. Only the scoped node leaf/key,
server CA, gateway leaf/key and public Hub CA were installed on A; the management
kubeconfig and CA signing keys remain on the control-plane host.

The package uses the canonical Python path `/root/fleet/python/bin/python3.11`;
`/opt/conda/bin/python3.11` is a symlink and is intentionally rejected by the
Raw supervisor's path check.

The extension controller now publishes tightly owned native Node and Cluster
heartbeat Leases from the authenticated ContainerNode observations. The core
controllers therefore provide the same `Online`, `Downgraded`, and `Offline`
aggregation used by Kubernetes clusters. The adapter also projects the Raw
capability and credential identity required by the core Raw Lease check, fenced
by the exact native and companion UIDs. It never changes `Node.spec`, Raw
reservations, or Docker reservations. Online means the connection is fresh;
it does not make a cordoned node schedulable. The effective scheduling decision
continues to check the native Node's `spec.unschedulable` and allocation state.
An idle Raw supervisor does not account for pre-existing Ray/Fleet GPU usage.

The control-plane host runs `rlark-container-controller`, `rlark-container-api`,
and `rlark-container-hub` as enabled user systemd services. User lingering is
enabled so logout does not stop them. The API listens only on `127.0.0.1:9444`;
A's private Agent listener is `10.1.3.100:9444`. A's three additive supervisord
programs are `rlark-raw-supervisor`, `rlark-container-gateway` and
`rlark-container-agent`; use targeted `status`, `start`, `stop` or `restart` for
these names only. Do not stop the trusted Raw supervisor to restart the Agent.
Notebook replacement still requires verified retirement and fresh enrollment.

Cleanup removed native Node `docker-local-test`, its local
`rlark-docker-agent-test` Pod, the Pending `ksyun-gpu-smoke-20261031` Job, and the
old `ksyun-test-1` ContainerNode/native Node. All three real clusters and both
existing Lumo2 Jobs were retained; no allocations were created on A. The remaining
container node is `ksyun-a`. Cleanup used controller-owned finalizers, not forced
finalizer removal.

This acceptance does not establish Lumo2 inference, Ray multi-host traffic,
Kubernetes Pod networking, or S1U connectivity.

## 2026-11-03 Ray relay feasibility validation

An isolated two-node RLinf/Ray 2.59.0 test on the development host completed
bidirectional `Channel` exchanges for 1 KiB, 1 MiB and 4 MiB payloads. It used
loopback addresses and dedicated temporary Ray processes; it did not use or
inherit `HTTP_PROXY`, `HTTPS_PROXY`, or `ALL_PROXY`, and cleanup was limited to
those test process groups. The existing Fleet Ray job remained running. This
proves the RLinf channel works when Ray can use a sufficiently large port pool;
it is not a cross-host or A-relay acceptance.

The current Docker Ray executor pins nine Ray service ports plus a configurable
worker range of at least one port (256 by default). Ray nodes connect directly
to several independently listening services, so the existing A/Hub API relay
on TCP 30000 cannot carry Ray traffic, and exposing only three TCP ports on A
does not satisfy the current executor's direct-port contract. A single-port
solution requires a Ray-aware multiplexing transport and per-node redirection
or connector support; it is not implemented as an RLark feature. No HTTP proxy
environment variables are part of the proposed solution.

An isolated cross-host proof of concept then placed a mutual-TLS `smux`
gateway on A:30001 and process-scoped TCP connectors on the two Ray nodes. Both
Ray nodes joined the same cluster, and the gateway recorded 25 bidirectional
streams over that one public listener, including Ray service, worker, and
Gloo-selected dynamic ports. The process-scoped adapter used `LD_PRELOAD` and
temporary credentials; this demonstrates transport multiplexing only and is
not a production implementation or an RLark-supported capability.

After A's SSH endpoint recovered, the cross-host RLinf `Channel` acceptance
passed with the Gloo interfaces pinned on both Ray nodes. The head actor used
`enp5s0` at `192.168.31.2`; the worker actor used `eth0` at `10.1.3.100`.
Both `RLINF_COMM_NET_DEVICES` and `GLOO_SOCKET_IFNAME` were set to that node's
interface before starting Ray, and both actors reported the expected IPv4
address. Gloo initialized in both directions and each of three payload sizes
(1 KiB, 1 MiB, and 4 MiB) completed 10 observation/action request-response
round trips. This resolves the prior IPv4/IPv6 mismatch for this tested setup.
The A:30001 gateway recorded 33 streams targeting 18 distinct Ray/Gloo ports,
while carrying all cross-host test traffic over the single public listener. No
Lumo2 inference or robot connectivity was tested.

The acceptance used Ray 2.59.0 on both nodes, but Python 3.11.16 on the
development host and 3.11.14 in A's Fleet environment; Ray emitted a patch
version mismatch warning. PyTorch versions also differed (`2.11.0+cu130` and
`2.10.0+cu128`). The transport and Channel exchange passed with these versions,
but production rollout should align Python and PyTorch builds before treating
this as a supported deployment combination.

Read-only inspection of the authorized A notebook found `/dev/net/tun`, but no
`CAP_NET_ADMIN` and no loaded kernel WireGuard module. A cannot currently host
a TUN-based WireGuard interface from this notebook environment. This does not
rule out a future platform change that grants the required capability; it does
rule out claiming that the current A installation can provide that overlay.

The isolated cross-host single-port Ray/RLinf Channel test passed, but it does
not establish automatic RLark provisioning, a robot path or production support.
It used no proxy environment variables and did not change the active Fleet
cluster. This remains a process-scoped PoC using `LD_PRELOAD` and temporary
credentials.

A previous follow-up attempt was blocked because A:22 reset connections during
SSH key exchange. After the host restart, SSH recovered and the test completed.
All test Ray processes, connectors, and the A:30001 gateway were stopped, and
test temporary directories were removed. A's existing Fleet Ray head (PID
592376, port 6390) remained alive. A:30001 is free after cleanup. A:30000 was
not touched; at the post-test check it had no listener, so its service should
be checked separately after the host restart.

Native connectivity acceptance on 2026-11-02 passed after applying controller
RBAC and restarting only the extension controller: `ksyun-a` became
`Online / AgentLeaseActive`, and `ksyun-containers` became `Online` with one of
one native Nodes online. Pausing only the local Hub let both Leases expire and
both native resources become `Offline / AgentLeaseExpired` (the reason applies
to the Node). Starting the Hub restored fresh observations and both resources
became Online automatically. A remained `spec.unschedulable=true`; no allocations
or test Jobs were created. Two-member Online → Downgraded → Offline → Online
aggregation is covered by controller tests, not multi-host cloud acceptance.

## Native connectivity projection

The standalone controller always runs the additive connectivity adapter. It
reacts to companion changes and repairs current state every 15 seconds. Before
publishing, it checks the current native and companion UIDs, tenant, authoritative
`Cluster.spec.agentNamespace`, runtime, current-generation validation/readiness,
and observed credential against the active credential. Registration alone never
produces a heartbeat. Busy state, target release readiness, and cordon remain
separate from connection status.

For each fresh authenticated observation, it maintains
`node-heartbeat-<native-node-name>` and the namespace's `heartbeat` Lease. Both
have a 45-second duration and retain the **original server-side observation
time**, not the adapter's current time. The Cluster Lease uses the newest valid
member observation. Core controllers derive Node Online/Offline and Cluster
Online (all registered native Nodes online), Downgraded (some offline), or
Offline (cluster heartbeat expired); Cluster cordon/drain still takes precedence.
Status convergence also depends on core controller reconciliation and UI refresh.

Leases have native controller owner references and a
`containers.rlark.io/native-heartbeat-projection` annotation containing the exact
companion UID. The adapter refuses an unknown Lease, a replacement native UID,
an ambiguous namespace/binding, or a foreign Raw projection. It does not delete
or adopt foreign resources. Raw Nodes receive only the adapter-owned `status.raw`
and `raw.rlark.io/credential-id` compatibility projection; existing conflicting
Raw capability must be recovered explicitly. This does not issue native Raw
credentials or authorize legacy workload execution. Credential rotation requires
a new authenticated observation before renewal. Container replacement requires
retirement and fresh registration, not editing the old instance binding.

On lost Agent/Hub/API connectivity, invalid identity, companion deletion, or an
adapter outage, renewal stops and the retained Leases expire. Reservations and
running processes remain intact. The next valid heartbeat restores connectivity
without uncordoning the node. No SSH tunnel or Agent reinstall is needed for
this projection. Apply the updated controller RBAC before restarting the
controller: it adds native Node metadata/status patches and heartbeat Lease
get/list/watch/create/update/patch permissions in agent namespaces, in addition
to the separate leader-election Role. The main UI needs only a refresh.

```bash
kubectl --kubeconfig=/path/to/management.kubeconfig apply -f \
  extensions/container-runtime/config/rbac/container-runtime-controller.yaml
make -C extensions/container-runtime build
# For this user-service installation:
systemctl --user restart rlark-container-controller.service
kubectl --kubeconfig=/path/to/management.kubeconfig -n ksyun-container-agents \
  get leases.coordination.k8s.io
kubectl --kubeconfig=/path/to/management.kubeconfig -n ksyun-container-agents \
  get nodes.rlinf.io ksyun-a
kubectl --kubeconfig=/path/to/management.kubeconfig get clusters.rlinf.io ksyun-containers
```

## Goal and constraints

An administrator manages a separate Kingsoft Cloud container cluster from the
RLark console. Gateway A installs node Agents on explicitly authorized B/C
instances over SSH, while each node owns its own execution, credential and GPU
allocation. RLark connects to A on TCP 30000; B/C connect to A on the private
network. SSH is for installation and repair, not a persistent transport. A may
also run its own Raw Agent. The current cloud notebooks do not have a Docker
socket or `CAP_NET_ADMIN`; Raw executes processes in the existing notebook and
does not create containers. Docker Engine execution requires a local socket.

Port 30000 availability through the cloud platform must be verified before
deployment. Public port exposure, TLS names, reachability, the existing
notebook's lifecycle and privileges must be established from current state.
Do not interrupt existing jobs, alter network settings or assume a local
control-plane IP is reachable from the cloud. The initial model test runs in
Passive mode, with no robot action dispatch.

## Interfaces and ownership

### First runnable communication component: restricted A/Hub relay

The extension adds a separate `container-runtime-gateway` binary with two
modes. `gateway` on A accepts private Agent TCP connections and an authenticated
TLS connection from the Hub. `hub` on the RLark control-plane host connects
outbound to A and forwards bytes only to one fixed local extension API address.
This is an Agent management-API transport, not a VPN, Kubernetes network, SSH
tunnel, or arbitrary TCP proxy.

Both sides require TLS 1.3, dedicated certificates, the peer CA and the exact
lowercase SHA-256 fingerprint of the peer leaf certificate. A's Agent listener
must bind an explicit private or loopback IP. The Hub destination is fixed in
its startup configuration and cannot be changed by an Agent. Connections and
per-direction idle time are bounded; the Hub reconnects with bounded backoff.
Agent-to-API mTLS remains end-to-end through A, so the gateway credential
cannot impersonate a node. The configured Hub API address must terminate the
node-scoped Agent mTLS API.

Build with:

```bash
make -C extensions/container-runtime build
```

Gateway A:

```bash
extensions/container-runtime/bin/container-runtime-gateway --mode=gateway \
  --agent-listen=10.1.3.100:9444 --hub-listen=0.0.0.0:30000 \
  --server-cert=/etc/rlark/gateway.crt --server-key=/etc/rlark/gateway.key \
  --roots=/etc/rlark/hub-ca.crt --peer-cert-sha256=REPLACE_HUB_LEAF_SHA256
```

RLark Hub:

```bash
extensions/container-runtime/bin/container-runtime-gateway --mode=hub \
  --gateway-address=REPLACE_A_PUBLIC_HOST:30000 \
  --server-name=gateway-a --api-address=127.0.0.1:9444 \
  --client-cert=/etc/rlark/hub.crt --client-key=/etc/rlark/hub.key \
  --roots=/etc/rlark/gateway-ca.crt --peer-cert-sha256=REPLACE_GATEWAY_LEAF_SHA256
```

Port 30000 is only the TLS Hub listener and must be explicitly allowed from the
RLark Hub source. Port 9444 is the private Agent listener in the accepted A
deployment and must remain private to A/B/C. These values illustrate the current test host's private
address only; query the platform again before deployment because its IP and
port mapping may change. The Agent must use an HTTPS endpoint whose host
resolves to A's private address on B/C and whose server certificate SAN matches
that host. Do not start the listener on the test notebook just because it has
an SSH connection; current read-only inspection found no Docker socket,
systemd or `CAP_NET_ADMIN` there. A short-lived listener probe confirmed that
TCP 30000 is externally reachable when a process listens on it; closed-port
refusal alone must not be treated as proof that the platform blocks it.

The package tests cover end-to-end node TLS, wrong pins, reconnection, idle
cleanup, bounded configuration and shutdown. This test does not establish
Kubernetes Pod networking, Ray multi-host traffic, Lumo2 inference or S1U
robot connectivity. Certificate rotation requires an administrator to update
the corresponding CA/pin and restart the process; automatic enrollment and
renewal remain unsupported.

Keep implementation in the independent container-runtime extension and limit
core integration to a narrowly scoped adapter. Introduce typed cluster gateway,
node enrollment and identifiable installation/retirement operations. An
administrator selects a Cluster, stable notebook ID or SSH address, pinned SSH
host identity, secret reference, explicit runtime, authorized GPU entitlement
and immutable Agent release. Persist desired state separately from one-shot
operations and observed connection and node status. API writes require
server-side administrator and tenant authorization. Credentials and workloads
do not appear in events, logs, CRD status or UI responses.

Gateway and nodes use separate node-scoped credentials. The gateway is not an
issuer or administrator and cannot impersonate nodes. Node Agents establish
authenticated connections via A; the hub validates current Cluster, Node,
instance, tenant, credential and allocation identities for every operation.
The gateway never provides an unrestricted network proxy. Require explicit
authorized task endpoints and resource limits. Automatic certificate
enrollment, renewal and dynamic address recovery are future implementation,
not existing features; the current ContainerNode spec includes an immutable
private address and instance ID. Instance replacement requires verified
retirement and re-enrollment, never silently editing immutable bindings.

## Lifecycle and tests

Install by retained operation identity: verify the cloud instance, obtain its
current address, validate its SSH host key, connect with the referenced
credential, inspect the runtime and authorized GPUs, and install a verified
release in a private directory. Wait for an authenticated fresh heartbeat;
retries must not take over foreign files or processes. Existing long-running
workloads and uncertain operations keep their reservations. Prefer systemd on
hosts where available; provide the explicitly managed foreground launcher for
notebook environments without systemd. The first enrollment of A itself must
be independently provisioned; it cannot install itself through an unavailable
gateway.

Reuse the existing opt-in idle rolling-upgrade and allocation fencing path;
verify artifacts and complete cleanup before advancing. Retire by blocking new
work, waiting for exact cleanup, revoking the node credential and removing only
proven owned resources. An offline node or uncertain cleanup remains blocked;
do not destroy the cloud notebook or other workloads.

Separate Gateway connectivity, Agent freshness, runtime availability,
schedulability and network readiness in UI/API. Validate unauthorized access,
stale credentials, replacement instances, response loss, competing operations,
restart, disconnect, partial installation, GPU authorization, deletion and
recovery. Exercise port 30000 and actual private cloud onboarding before
declaring the gateway supported.

The first data-plane milestone is an explicitly authorized bounded TCP service
relay for cloud Lumo2 inference and S1U Passive observation. An Agent heartbeat
does not prove this route works. Raw is not currently an RLark Domain endpoint,
and the current Fleet interactive Env is not automatically a WebSocket client;
implement and test the adapter, endpoint identity, and bidirectional path before
any cloud/robot claim. Multi-node Ray/RLinf and full Pod network connectivity
are separate later gates. Validate the specified 30000-step checkpoint against
its own processor, BAT assets, normalization and model interface; no Active
robot motion is authorized by the Passive test.
