# Container gateway and managed enrollment plan

Status: partial implementation. The independent extension now has a tested,
optional A/Hub mTLS relay for the fixed Agent API described below. SSH-managed
node installation, UI enrollment/retirement operations, and the cloud-to-robot
data path remain design/acceptance work; this plan is not complete.

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
  --agent-listen=10.1.3.100:30001 --hub-listen=0.0.0.0:30000 \
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
RLark Hub source. Port 30001 is the private Agent listener and must remain
private to A/B/C. These values illustrate the current test host's private
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
