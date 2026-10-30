# Container runtime management console

The container runtime extension has an independent administrator console. It
is a companion to the native RLark console and is packaged under
`extensions/container-runtime/`; it does not add container fields to the
native Cluster, Node, Job, or Task APIs.

## Scope of this increment

The console is a preview management surface for explicitly provisioned Raw
container clusters. It provides:

- a cluster list and detail view;
- authoritative node identity, private address, runtime, capabilities,
  authorized GPU UUIDs, heartbeat age, readiness, schedulability, and busy
  state;
- available Agent releases and the currently selected target release;
- operation-safe cluster creation, target-release selection and node cordon/uncordon.

Allocations, their Job references, and Agent upgrades are displayed as separate dimensions.
The console never derives utilization from a missing observation and never
turns a stale heartbeat into a Ready or schedulable state. Ray/RLinf group
submission, terminal/exec, automatic enrollment, Docker Engine creation, and multi-node
scheduling remain outside this preview. Bounded Raw log reads are available only
when the separate loopback log listener and node poller are explicitly enabled.

## Resource and ownership model

The extension owns `containers.rlark.io/v1alpha1` companion resources. A
`ContainerCluster` points to one native Cluster by name, namespace, and UID;
`ContainerNode` points to the exact native Node and Cluster instances. The
server verifies those references, the authoritative `Cluster.spec.agentNamespace`,
and the independent tenant before returning a record or accepting a write.
Names and labels are lookup hints only. A replaced native object is a conflict
until an administrator explicitly provisions a new companion object.

Cluster creation is idempotent only for the same canonical operation UUID and
request fingerprint. A retry may recover a committed namespace or companion;
it cannot adopt an object with different ownership metadata. Selecting a target
release requires the current Cluster resourceVersion, the exact release UID,
the release's current `Available=True` observation, and the same tenant. The
operation is a durable desired target: clearing or replacing it changes future
rollouts and does not cancel an Agent already applying an upgrade.

## Authentication and deployment

The browser uses the normal RLark authenticated session through a same-origin
Gateway route. The extension API accepts only a short-lived RLark HS256 JWT
issued by the trusted Gateway, requires issuer, subject, administrator role,
algorithm, and expiration, and rejects user-supplied identity headers. It does
not issue browser tokens, accept kubeconfigs, expose signing keys, or proxy an
arbitrary Kubernetes path. The API process uses a dedicated read/write-limited
management ServiceAccount and a separate Agent mTLS endpoint; Agent
certificates are never sent to the browser.

Serve the independent UI and API from the same origin under
`/api/v1/container-runtime/`. Use TLS or a loopback reverse proxy in
development. Set `Cache-Control: no-store` on authenticated responses and do
not log authorization headers, kubeconfig contents, certificate material, or
task payloads. A direct end-user request without an administrator JWT receives
401; an authenticated non-administrator receives 403.

The browser receives typed summaries only. It never receives raw Secret data,
certificate paths, private keys, or management kubeconfig data. The initial
node join and package export remain administrator CLI operations; the UI can
show the resulting node state without becoming an enrollment channel.

## UI behavior and failure handling

The cluster page separates desired state from observations:

| Dimension | Examples |
| --- | --- |
| Desired state | runtime, target Agent release, pause/rollout intent |
| Identity | native/companion UID, tenant, namespace, private address |
| Observation | heartbeat age, Ready, schedulable, busy, capabilities |
| Capacity | CPU/memory/GPU totals and authorized GPU UUIDs |
| Operations | allocation count, upgrade phase, last verified receipt |

Every refresh is cancellable and stale responses are discarded. API errors
retain their machine-readable HTTP class and show “data unavailable” rather
than zero counts. A stale node is visible and blocked from scheduling; it is
not silently removed. Writes use an optimistic resourceVersion and report a
conflict that asks the administrator to refresh.

## Supported deployment sequence

1. Install the extension CRDs and scoped API/controller RBAC.
2. Create a dedicated native Cluster and container companion with
   `container-runtime-admin`.
3. Register each existing Raw container host and export its bounded Agent
   package. The host runs the trusted root process-tree backend when cgroup-v2
   or systemd is unavailable; this is not a task isolation boundary.
4. Start the extension controller, Agent API, and independent console. Enable
   Job reservations and task execution only for an isolated preview cluster.
5. Open the console, verify the node's fresh heartbeat and authorized GPU
   inventory, then publish an available Agent release as the desired target.

The current Kingsoft Cloud acceptance is one private H100 node in the
`regular-training` queue, with 20 CPU, 236 GiB memory, one GPU, and no public
IP. The selected replacement image is `rlinf-dashboard:1.1`; the image is a
starting point and does not prove that every package installed on the source
machine is present. Single-process Raw GPU lifecycle and one-node idle
rollout are accepted. Multi-node Ray/RLinf and Docker Engine lifecycle require
separate conformance tests before being advertised.


## Build and run the independent console

The console uses Bearer authentication only. Its login form forwards typed
credentials to the configured Gateway; the returned administrator token remains
in browser memory and disappears on reload or logout. Gateway cookies are not
forwarded. The console is a separate origin/session from the native UI, and no
native navigation entry is installed in this increment. The administrator role
has platform-wide inventory access across tenants; ordinary users have none.

Provision `config/rbac/container-runtime-console.yaml` from the extension
folder. The example ServiceAccount lives in `rlark-system`; change all three
references together for a different deployment namespace. It can read inventory,
create native Clusters and dedicated namespaces, and create/patch companion
Clusters. It cannot read Secrets, dispatch Tasks, patch observed status, delete
resources or release allocations. Native Node patches are limited by the API
to `spec.unschedulable`; Kubernetes RBAC cannot restrict a patch permission by
field, so the console remains a trusted administrator service. Keep the signing key out of that kubeconfig.

The HS256 key must be the exact raw bytes used by the configured Gateway (at
least 32 bytes), mounted read-only into this trusted control-plane service.
HS256 is symmetric: possession of this key also permits signing tokens, so the
console has the same token trust level as Gateway. Key distribution is an
operator responsibility; rotate/restart both services together. The console
never obtains the key through Kubernetes Secret permissions.

```bash
cd extensions/container-runtime
make build
make build-ui
bin/container-runtime-console \
  --addr=127.0.0.1:9445 \
  --kubeconfig=/private/console.kubeconfig \
  --gateway-jwt-key-file=/private/gateway-jwt.key \
  --gateway-origin=http://127.0.0.1:19000 \
  --ui-directory=ui/dist
```

Open `http://127.0.0.1:9445/` and sign in as an administrator. The command accepts
HTTP only on an explicit loopback IP. Off loopback, supply `--tls-cert` and
`--tls-key`; a remote Gateway origin must also use HTTPS with a trusted system
CA. No insecure-TLS flag is provided. Node mTLS traffic continues to use the
separate `container-runtime-api` service. The UI build requires Node.js
20.19+ or 22.12+; Node.js 24 was used for acceptance.

The preview UI creates Raw clusters. API/CLI provisioning can also create
Docker inventory records, but that does not enable Docker Engine registration
or execution. Join/package export, artifact publication and node drain
remain CLI/operator workflows. Job names in the console come from allocation
references; the console is not a Job submission or history interface.

The page refreshes observations every 15 seconds. Heartbeats older than 45
seconds, future heartbeats, stale generations, credential mismatches, replaced
native nodes, cordon/drain, legacy reservations, held locks and target-version
mismatches fence the displayed scheduling eligibility. A snapshot is an
indication only: the allocation API revalidates authority at dispatch. The UI
shows registered authorization and last observed capacity, not live utilization
or guaranteed free GPU capacity. Changing target may fence older nodes according
to the existing scheduler policy; it does not guarantee service availability.

Creation retains the canonical request/operation UUID in sessionStorage (no
credentials). After an uncertain result the form retries that exact request.
Do not clear it and start a different operation to repair a partial create;
inspect the existing native/namespace/companion ownership first. Clearing a
target requires the same Cluster UID and resourceVersion as setting one.
Release selection checks its exact UID/current availability, but references
can change after the read: rollout and Agent controllers must recheck them
before any effect. Errors return bounded `code`/`message` fields and an
`X-Request-ID`; they do not return upstream credential/configuration details.

## Validation recorded on 2026-10-31

Go tests, vet, the extension build and incremental Go lint pass. UI transport
and stale-read tests, TypeScript checking, production build and dependency
audit pass. Browser acceptance against the real Gateway and KCP used the
restricted console ServiceAccount: administrator login, real `ksyun-containers`
inventory, non-administrator rejection, a dedicated temporary cluster create
with the first committed response deliberately dropped, identical-UUID retry,
target-release publication, English/Chinese switching and logout. The browser
reported no JavaScript exceptions. Temporary fixture resources were removed
through normal lifecycle cleanup; the existing cloud Cluster target was not
changed. This does not validate multi-node training or production deployment.

## Node scheduling management

The console adds administrator cordon/uncordon for existing Raw
nodes. It patches only native `Node.spec.unschedulable`, with the companion UID,
native UID reference, and native resourceVersion checked on every request. The
console displays the native scheduling intent separately from observed Ready,
busy state and held allocations. Cordon blocks subsequent start authorizations; it does not stop a
running process, release its allocation, or request an Agent upgrade. Cordon
does not pause the configured Agent rollout; upgrades still wait for their
independent idle proof and lock. It cannot revoke a start authorized concurrently
just before the cordon was observed. Uncordon
clears the intent only after all known locks and legacy reservations are gone;
it does not grant readiness or bypass placement/upgrade checks. A stale or
unknown lock, including one being deleted, blocks uncordon. Failure to read
reservations also blocks it.

Use `PATCH /api/v1/container-runtime/clusters/{namespace}/{cluster}/nodes/{node}`
with `uid` (companion Node UID), `resourceVersion` (native Node version) and an
explicit boolean `unschedulable`. Re-read after a conflict or uncertain response;
never retry a toggle against a new object. Node writes require the same
platform-administrator authentication as cluster writes. Additional console
RBAC grants only native Node `patch`, never Node status writes.

Drain is a separate future operation: stopping Jobs, proving cleanup and
retiring allocations must be recoverable. No drain or force-unlock button is
provided by this scheduling-intent endpoint.

## Recovery after a notebook restart

A platform stop/start can replace the existing-container incarnation even if the
notebook ID, private address and GPU UUID stay the same. Compare the current PID
namespace, PID 1 start evidence and boot identity with the registered `instanceID`.
Do not reuse old credentials or mutate the immutable binding to adopt a replacement.
After proving the old instance stopped and its Tasks, allocations and upgrade
operations absent, retire the exact old native/companion Nodes using
UID/resourceVersion preconditions, register the replacement and export a new
package into separate install/state directories. The Cluster remains separate
from local Kubernetes inventory. Automatic enrollment and startup recovery are
not implemented; a temporary reverse tunnel must also be re-established.

On 2026-10-31 the test notebook was observed stopped and explicitly restarted
without changing its queue, resources or public-network settings. Its PID 1 start
evidence changed. Python/PyTorch/Ray and CUDA `42.0` remained available; the old
node was normally retired and a new exact registration/package created. This
operator recovery is separate from Agent binary rollout and is not automatic.


## Bounded Raw log preview

The console can show a retained Raw Task snapshot when the API is started with
`--log-read-addr=127.0.0.1:9446` and
`--gateway-jwt-key-file=/private/gateway-jwt.key`. The Agent package must be
exported with `enableExecution: true` and `enableLogReads: true`; the Agent
polls the existing mTLS endpoint and the supervisor reads only the exact retained
Task log. `--log-read-addr` accepts an explicit loopback address only. The
console uses `--log-api-origin=http://127.0.0.1:9446` to proxy the fixed
`/api/v1/container-runtime/logs` route.

A read requires the allocation UID and Task UID from the current inventory. It
returns at most 64 KiB of combined stdout/stderr, from the beginning of the
retained file, with truncation metadata. Requests time out after 20 seconds,
are limited to one per node and 32 globally, and are discarded on API restart.
Cordon, suspension and stale readiness do not revoke exact diagnostic access;
credential revocation, object replacement, allocation release and ambiguous
ownership do. The listener never accepts a path, stores log bytes in Status,
forwards cookies or follows redirects. Workload output is rendered as escaped
plain text; historical archives, follow/tail, stats and terminal/exec are not
part of this preview.

The private-cloud acceptance on 2026-10-31 exercised this route through the
console proxy on the dedicated `regular-training` H100 notebook. The test
authenticated with a short-lived Gateway token, rejected unauthenticated,
malformed-path and wrong-Task-UID requests, then read 65,536 bytes containing
the CUDA marker and truncation metadata. After normal stop and allocation
release, the old allocation identity was rejected. The runner used the
management kubeconfig only; restricted controller credentials cannot create
native Jobs.
