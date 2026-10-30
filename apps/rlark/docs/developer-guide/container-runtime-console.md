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
- operation-safe cluster creation, target-release selection and node cordon/uncordon;
- explicitly enabled single-process Raw Job submission, bound Job lists, and durable suspension.

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
certificate paths, private keys, or management kubeconfig data. When
`--enable-node-registration=true` and the additive registration RBAC are
installed, an administrator may declare an existing Raw node from the selected
cluster page. Certificate/package export remains on the administrator CLI and
the browser never becomes an enrollment or signing channel.

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

During a console upgrade, the UI accepts older inventory responses that omit
`jobSubmissionEnabled` and treats submission as disabled. An explicitly supplied
capability must still be a boolean; malformed inventory remains an error.

## Supported deployment sequence

1. Install the extension CRDs and scoped API/controller RBAC.
2. Create a dedicated native Cluster and container companion with
   `container-runtime-admin`.
3. Optionally register each existing Raw container host from the administrator
   console (or CLI), then export its bounded Agent package. The host runs the trusted root process-tree backend when cgroup-v2
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
  --ui-directory=ui/dist \
  --enable-node-registration=true
```

Before enabling this flag, apply `extensions/container-runtime/config/rbac/container-runtime-console-registration.yaml` together with the base console role. Remove the additive binding when registration is disabled.

Open `http://127.0.0.1:9445/` and sign in as an administrator. The command accepts
HTTP only on an explicit loopback IP. Off loopback, supply `--tls-cert` and
`--tls-key`; a remote Gateway origin must also use HTTPS with a trusted system
CA. No insecure-TLS flag is provided. Node mTLS traffic continues to use the
separate `container-runtime-api` service. The UI build requires Node.js
20.19+ or 22.12+; Node.js 24 was used for acceptance.

The preview UI creates Raw clusters. API/CLI provisioning can also create
Docker inventory records, but that does not enable Docker Engine registration
or execution. Package export, artifact publication and node drain remain CLI/operator workflows;
registration is available only through the explicitly enabled administrator route. Allocation rows retain their Job references; the
optional Raw Job panel separately lists verified bound Jobs, submits one process
and records suspension intent. It is not a historical archive.

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

The Agent poll endpoint and log listener must belong to the same API process:
pending reads are held in memory there. Pointing log reads at another instance
times out even when both instances share the management API. This preview has
no cross-instance request routing.

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

## Raw Job submission increment (preview implementation)

Add an explicitly enabled administrator-only submission surface at
`POST /api/v1/container-runtime/clusters/{namespace}/{cluster}/jobs`.
The strict request carries a canonical operation UUID, the exact companion
Cluster UID, prepared environment, executable argv, and working directory.
The server derives tenant, native Cluster and namespace from current bindings;
clients cannot select another tenant, inject a Pod template, set GPU visibility,
or request Docker/Ray/group dispatch. This creates a single Raw process Job;
whole-node/authorized-GPU reservation remains the existing controller's job.
Submission accepts queued work; it does not promise capacity or verify local
executable/dependency availability. The supervisor still enforces its path policy.

The operation UUID determines one native Job and companion binding name. Create
the Job suspended, create its exact-UID ContainerJob, then activate by one atomic
UID/resourceVersion-tested patch that also records activation. Retry identical
input after response loss; compare the request fingerprint, full workload and
ownership before every write. A retry must never resume a Job suspended after
activation or rerun a completed Job. Existing bindings whose Job is missing or
replaced fence recreation. Before a binding exists a suspended Job has no
execution authority. Partial failures retain objects for the identical request
to resume; do not delete/recreate or submit a fresh UUID to repair uncertainty.
Idempotence lasts while either record is retained; administrator deletion of
both records explicitly ends it. No background submission worker is added.

Provide a typed list of this cluster's bound Jobs, and a separate durable
suspension request with exact Job UID/resourceVersion. Suspension is not immediate
cleanup: display Job phase and condition freshness separately and wait for the
existing allocation cleanup workflow. No force unlock, restart, automatic retry
or deletion action is added. The console needs additive Job/ContainerJob RBAC
and an explicit `--enable-job-submission` flag; the default remains disabled. Apply
`extensions/container-runtime/config/rbac/container-runtime-console-jobs.yaml`
alongside the base console role, then add the flag to the existing console
command. This role grants native Job get/create/patch and companion Job
get/list/create; it grants no deletion, Task dispatch or status writes. Kubernetes
RBAC cannot limit Job patches to suspension, so this is a trusted administrator
service. The detail response advertises `jobSubmissionEnabled`; disabled or
unsupported clusters do not offer submission or poll the absent Job endpoint.

The form takes argv and environment as JSON string arrays, preserving spaces
and empty arguments without shell parsing. The executable and working directory
must be canonical absolute paths. GPU visibility overrides are rejected. The
form retains an uncertain request's operation ID and exact cluster UID, disables
cluster switching and retries the same payload. Job phase is shown separately
from suspension intent and current-generation observation. Invalid bindings and
terminal Jobs disable the suspend action.

The list uses `GET` on the same `/jobs` route. Suspend uses
`PATCH .../jobs/{job-name}/suspend` with `uid` and `resourceVersion`. Conflicts
return 409 and require re-reading; invalid submissions return 400; management
outages return a sanitized 503. No endpoint resumes an already suspended Job.

Example POST body (replace identities, profile and paths using current inventory):

```json
{
  "operationID": "d07faab0-5896-4b9a-af29-887ba579c69f",
  "clusterUID": "REPLACE_COMPANION_CLUSTER_UID",
  "environment": "REPLACE_REGISTERED_PROFILE",
  "process": {
    "argv": ["/usr/bin/python3", "train.py"],
    "env": [],
    "directory": "/work"
  }
}
```

Browser submission intents stay in memory because argv may contain secrets.
After an uncertain response preserve the form/UUID and retry unchanged; after a
page reload use the retained operation ID and administrator inspection. Log only
operation/resource identity, never argv/environment or upstream errors.

Validation must cover unauthenticated/non-admin access, wrong/replaced/ambiguous
Cluster bindings, unsupported runtime, invalid paths and GPU overrides, partial
create and activation response loss, concurrent retries, stopped/completed Job
replay, conflicting ownership/spec, list isolation, stale suspension, UI request
validation/cancellation, and real private-node dispatch/cleanup before advertising
cloud submission acceptance.


### Submission acceptance on 2026-10-31

The dedicated `regular-training` notebook was restarted after platform stop,
without changing its 1 GPU / 20 CPU / 236 GiB or public-network settings. Current
container evidence was rechecked, the stopped old exact Nodes were retired only
after Task/allocation/upgrade absence, and a new registration/package was issued.
PyTorch 2.11.0+cu126, Ray 2.56.1 and CUDA remained present; no dependency install
was needed. Temporary SSH multiplexing/reverse forwarding restored test access.
This remains an operator recovery, not automatic restart enrollment.

Against real Gateway/KCP with the restricted console ServiceAccount, a browser
submitted a Raw process, deliberately dropped a committed POST response, retried
the exact in-memory intent, and recovered the same Job UID. It observed Running,
suspended through the page with exact UID/resourceVersion, waited for Stopped and
allocation deletion, and proved identical submission replay preserves the stop.
Retained supervisor logs contained `RLARK_CONSOLE_GPU_OK` only after one-GPU CUDA
computation returned `42.0`. EN/CN labels passed with no JavaScript exceptions.
Dedicated acceptance Jobs/bindings/Tasks were normally cleaned with exact
preconditions. The node returned Ready, idle and schedulable, with no allocation.
This establishes only the single-process Raw submission preview; distributed
Ray/RLinf and Docker Engine Job submission remain outside its scope.

The extension test/vet/build, submission/console race tests, eleven UI transport/
model tests, production UI build and strict bilingual documentation build pass.
Repository `make test-go` passes. Incremental lint reports zero issues; full
repository lint remains blocked by thirty existing findings in native RLark.

## Administrator node registration

This increment adds opt-in registration of an existing Raw container from its
selected cluster detail page. The API/UI and failure/recovery tests are covered;
cloud enrollment and Agent package acceptance remain separate operational gates.
Enable `--enable-node-registration=true` and install the additive
`extensions/container-runtime/config/rbac/container-runtime-console-registration.yaml`.
The default console has no registration route or Node-create permission. The
additive role grants only native Node and ContainerNode creation; it grants no
Secrets, signing, status, delete, dispatch or credential export permissions.

`POST /api/v1/container-runtime/clusters/{namespace}/{cluster}/nodes` requires an
administrator JWT and a strict JSON body:

```json
{
  "operationID": "5b70ee0b-563c-4bc9-8117-58e80ba70f75",
  "clusterUID": "replace-with-current-containercluster-uid",
  "name": "cloud-gpu-2",
  "instanceID": "replace-with-verified-container-incarnation",
  "privateAddress": "10.0.3.86",
  "environment": "rlinf-pi05",
  "networkScope": "ksyun-private",
  "authorizedGPUUUIDs": ["GPU-replace-with-platform-authorized-uuid"]
}
```

`clusterUID` is the companion UID precondition, not the native Cluster UID.
The server resolves the exact native binding, namespace and independent tenant;
client-supplied tenant/native references are rejected. Registration supports only
Raw existing-container mode. Verify the actual incarnation and platform GPU
entitlement first; visibility alone does not authorize a GPU. Duplicate private
addresses in the same network scope and duplicate GPU UUIDs are conflicts.
The service rechecks the companion UID as well as the native binding. The CLI's
optional `containerClusterUID` precondition pins the same companion instance;
omitting it preserves existing CLI requests and their operation digests.

A successful response contains `namespace`, `name`, companion `uid`, `nativeUID`,
`clusterUID` and `operationID`. It creates no Ready observation, certificate,
package or process and cannot release an existing cordon. Use those exact node
identities with administrator `node-package` on the trusted provisioning machine;
its `credentialID` is the registration operation UUID. Keep the CA private key
there. Transfer/install the package privately, then verify a fresh authorized
heartbeat before scheduling.

The browser keeps the exact request in memory before its first POST. An uncertain
response, HTTP 409 or unavailable service retains it and disables cluster switching
and concurrent Job/cluster creation. Retry uses the identical operation UUID and
input. Only pre-effect HTTP 400 validation permits editing a new request. Refresh,
logout or closing the browser loses the in-memory request: retain the operation
UUID and input privately and inspect partial resources before starting another
operation. A registration response with mismatched node, cluster or operation
identity is an uncertain result, never a successful join. Older APIs omitting
`nodeRegistrationEnabled` disable the form; a supplied non-boolean is an error.

## Independent Raw template scheduling

The opt-in reservation controller accepts up to 16 uniquely named independent Raw
process templates in a native Job, with separate allocations and aggregate cleanup.
The console submission API and form still create one process template. To exercise
multiple independent members, use the administrator management-API procedure and
examples in the [delivery plan](raw-container-runtime-plan.md). Partial admission
is allowed; this is not Ray/RLinf distributed training or a gang scheduler.

A two-template Job may execute sequentially on one eligible node; a cleaned, released sibling does not block the next member. Starts still require the member's exact current allocation, and whole-set retirement still blocks new starts.
