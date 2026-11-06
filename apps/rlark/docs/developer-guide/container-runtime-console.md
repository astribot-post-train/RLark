# Container runtime management console

The container runtime preview is integrated into the RLark administrator UI at
**Clusters → Container Clusters** (`/admin/container-clusters`). It shares the
normal RLark login, language, theme and same-origin Gateway. The page lives in
`apps/rlark-ui/src/admin/container-runtime/`; management logic stays in the
independent `extensions/container-runtime/core` Go module. A narrow Gateway
adapter mounts its typed handlers. No container-specific fields are added to
native Cluster, Node, Job or Task APIs.

## Scope of this increment

The console is a preview management surface for explicitly provisioned Raw and
Docker container clusters. It provides:

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
scheduling remain outside this preview. Docker node registration and idle Agent
rollout are available when their additive capabilities are enabled. Bounded Raw log reads are available only
when the separate loopback log listener and node poller are explicitly enabled.

## Gateway topology and service management preview

Container node records distinguish a logical Cluster Gateway from Worker
nodes. This role describes RLark's container topology; it does not create a
Kubernetes control plane. A Gateway may also execute workloads. Agent heartbeat
and native Node online status remain separate observations. Neither status
proves A-to-Worker SSH reachability or Ray data-plane connectivity.

The selected Agent release is immutable for a provisioning attempt; the UI may
default to the newest available compatible release. A node registration may
provide an explicit GPU UUID allowlist. When that list is empty, the authenticated
Agent's latest visible GPU inventory is used as the node's authorized inventory.
This automatic mode requires a non-future heartbeat no older than 45 seconds;
missing or stale observations fail closed for scheduling. Visibility is treated
as entitlement only for these explicitly registered container nodes, so use it
only when the Notebook's device view is the intended allocation boundary.

The node table separates Gateway/Worker role, native online status, Agent
heartbeat and SSH reachability. The opt-in Notebook start/join preview accepts
addresses and roles, pins the selected available release, then installs the
Agent/task manager and A's gateway using strict preconfigured SSH host trust.
An operation waits for an authenticated fresh Agent heartbeat after process
startup; a live PID or SSH probe alone is not readiness. Use the same retained
operation ID to recover an uncertain request. The preview still requires a
private controller enrollment profile, published artifacts and additive RBAC;
real A/B/C installation and recovery acceptance is outstanding.

Registration alone remains metadata-only. Stopped Notebooks must first be
started in the cloud console. GPU keepalive is not part of enrollment or the
Agent package.

A restarted Notebook is a new container with a new instance identity and
private address. To bring it back, submit an enrollment with the node's name
and SSH endpoint (update the address only if the endpoint changed). The newest
SSH probe is authoritative: when the existing record of that name describes a
different instance, private address or role, the controller retires it (the
ContainerNode, then its native Node, each deleted with a UID precondition) and
registers the probed container under the same name. A record still held by an
allocation or upgrade is kept until the hold clears; the operation waits with
`ReplacementBlocked` and resumes automatically. A same-name record of another
cluster or tenant is never touched and fails with `NodeIdentityConflict`.
Worker requests carry only name, role, SSH address and port; a Gateway keeps its
record UID so a verified running gateway can be reused. Replacement requires
the controller's `delete` permission on `containernodes` and `rlinf.io` `nodes`.

After an enrollment succeeds, later SSH probes update the node's reachability
and reason without rewriting the successful operation result or starting any
services. `NotebookReplaced` and `PrivateAddressChanged` on a completed
operation report that the node has since restarted; submit again to replace
its record. `InvalidProbe` makes reachability unknown. The probe can recover on a
later matching observation, even if the historical release has been removed.
This behavior requires the updated enrollment controller; successful enrollment
is historical evidence, not proof that the current node or Agent is ready.
Existing independently installed services are not changed by removing package
support; check their ownership before any operational cleanup.

When a new enrollment explicitly pins an existing Gateway's ContainerNode UID
and its authenticated Agent reports the selected release version, the updated
controller can reuse that Gateway without reinstalling or restarting it. It
requires a fresh, current-generation Agent heartbeat and an SSH read-only check
of the same Notebook instance, private address, private service configuration,
PID/start-time receipts and running Agent executable digest. The Agent, gateway
and task-manager must all be observed. A stale Agent yields
`ExistingAgentNotReady`; an unverifiable service set yields
`ExistingGatewayUnverified`. Both wait for recovery and never fall back to
reinstalling that Gateway. This proves an observed service set, not data-plane
or workload readiness. The updated controller must be deployed for this path.

The enrollment package records the authenticated Notebook instance fingerprint
(PID namespace, PID 1 start time and boot ID) and its private address. The local
service manager checks both before starting or adopting a process receipt. A
changed fingerprint or address fails closed; use an explicit replacement
operation to issue a new node identity and package. Completion requires the
Ready condition itself to observe the current node generation, as well as a
fresh heartbeat and the pinned Agent version.

Trusted enrollment profiles may use `$GATEWAY_PRIVATE_ADDRESS` in `endpoint`
and launch arguments, `$NODE_PRIVATE_ADDRESS` in launch arguments, and
`$INSTALL` for the private package directory. Values come from authenticated
Notebook discovery; the platform SSH/DNAT address is not the container address.
Unknown placeholders fail package creation. Static profiles remain accepted.
After an address change, explicitly replace the node and ensure the API TLS
certificate covers the newly discovered gateway address before installing.
This expansion does not rotate certificates or rewrite arbitrary configuration
files in `gatewayFiles`.

The trusted profile's optional `ssh.ControlDirectory` enables OpenSSH connection
reuse for bootstrap only. Use a dedicated canonical directory with mode `0700`;
symlinks, public directories and paths too long for a Unix control socket are
rejected. Connections are scoped by the
OpenSSH `%C` host/user/port hash, retain strict host trust, and expire 60 seconds
after the last bootstrap command. Worker keys stay on A. This is installation
transport, not an inference tunnel; established connections surviving an
intermittent SSH handshake failure do not prove the public endpoint is healthy.

Profiles may override the default workload allowlists by node name using
`executablesByNode` and `workDirectoriesByNode`. These are trusted server
configuration, not UI-supplied paths. Paths must already exist and resolve to
themselves on that Notebook; resolve Python virtualenv symlinks explicitly when
configuring the profile. The service manager validates them before starting
services and creates only the node's private task/updater state directories.
Missing paths, public state directories or symlinks fail startup; they are not
silently dropped or rewritten. Workload paths remain separate from SSH IPs.

Task and upgrade reconciliation errors after a successful heartbeat are logged
and retried at the normal heartbeat interval. They do not trigger the liveness
outage backoff or keep a stale observation alive. Observation or heartbeat
publication failures still use bounded backoff. Online therefore remains a
connectivity observation; task execution and upgrade readiness may be failing
independently. Deploy the updated Agent binary to use this behavior.

Use **Cancel enrollment (retain joined nodes)** to release an operation waiting
for a stopped or replaced Notebook before submitting a new topology. The
administrator route is
`POST /api/v1/container-runtime/clusters/{namespace}/{name}/enrollments/{operation}/cancel`
with `{"uid":"<NodeEnrollment UID>","clusterUID":"<ContainerCluster UID>"}`.
Cancellation is an irreversible Spec intent, not deletion: the controller marks
the operation Cancelled and does no further bootstrap work. Joined nodes remain
running. An SSH command issued before cancellation may still complete; inspect
its node receipts before a replacement. Retrying cancellation is idempotent.
Terminal success/failure is preserved. Direct management API clients may set
`NodeEnrollment.spec.cancelRequested: true` with the same semantics. Apply the
updated CRD and enrollment RBAC before deploying the controller/Gateway.

See the [delivery checklist](notebook-fleet-delivery.md) for the remaining
one-click lifecycle, transport automation and mixed Fleet acceptance gates.

## Native online status

The extension controller projects authenticated, current ContainerNode
observations into UID-owned native heartbeat Leases. Native Node and Cluster
pages now use the core controllers' Online/Offline and Online/Downgraded/Offline
aggregation. The 45-second Lease uses the original server-side heartbeat time;
stopping the Hub or adapter stops renewal, and the next valid observation
restores online status. Online and schedulable are separate: native Node cordon,
workload reservations, runtime capability and release readiness still apply.
The adapter owns only its Leases and fenced Raw capability/credential projection;
unknown Leases or conflicting native identity are rejected. It does not alter
Node Spec or reservations. Apply updated controller RBAC and restart the
extension controller; refreshing the existing UI is sufficient. See the
[connectivity projection contract and acceptance](container-gateway-plan.md#native-connectivity-projection).

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

The browser uses the normal RLark authenticated session and shared transport.
Gateway verifies the JWT and administrator role before establishing a typed,
in-process administrator identity for extension handlers. Client identity
headers cannot establish that identity. Unauthenticated requests receive 401;
authenticated non-administrators receive 403. Administrators have platform-wide
container inventory access, including across tenants; ordinary users have none.
All enabled routes are explicitly registered under `/api/v1/container-runtime`.
Authenticated responses use `Cache-Control: no-store`.

Gateway uses its configured management API credentials and an uncached client
for ownership and resourceVersion checks. Install the additive management RBAC
for that identity; the example binds `rlark-gateway` in `rlark-system`. Adjust
the subject to the actual Gateway identity, not the Agent API identity. The
browser receives typed summaries, never Secret data, kubeconfigs, Agent
certificates or signing keys. Registration and Job submission each require a
separate opt-in flag and additive role. Node patches remain restricted by the
handler to `spec.unschedulable`; Kubernetes RBAC cannot restrict patch by field.

Agent mTLS traffic still goes to the separate `container-runtime-api` service.
Optional diagnostics use its loopback listener and revalidate the same Gateway
JWT. Only that private listener needs the Gateway signing-key file; normal
management no longer distributes the signing key to another console process.
Do not log credentials or task payloads. No arbitrary Kubernetes proxy is exposed.

## UI behavior and failure handling

The page uses the shared RLark content viewport for vertical scrolling. A
compact cluster selector sits under the page heading, followed by an overview
card (identity, state and the release target control), a strip of four charts
(node readiness, A-to-node reachability, Raw Job outcomes and rollout
conditions) and one tabbed table for Nodes, Raw Jobs, allocations, releases and
upgrades. Chart segments always show a text label and count, so state is not
conveyed by color alone. Helper text and condition reason/message appear as
keyboard-focusable tooltips. Create cluster, Notebook deploy/edit, node
registration and Raw Job submission open in a side drawer; closing the drawer
does not discard an unconfirmed request or its retained operation ID. Each node
row offers **Edit IP / redeploy**, which opens the deploy drawer focused on that
node's SSH address; resubmitting after a restart replaces the node's old record
automatically (see above). The **Enrollments** tab lists
every enrollment operation with per-node reachability and phase, and offers
cancellation for operations still in progress (joined nodes are retained). The
controller runs one cluster's enrollments in creation order, so while an
operation is unfinished the deploy drawer shows its progress instead of the
form: one row per node with reachability, phase, running services and an
explained failure reason (for example, SSH from A to the node is unreachable),
refreshed every 15 seconds, plus the count of queued duplicate operations. This
prevents a second submission from silently queuing behind the first. **Cancel
and resubmit** cancels the current and queued operations, keeps joined nodes,
and reopens the form; **Edit IP / port and retry** on a failing node does the
same and focuses that node's address. Request errors appear in a banner at the
top of the drawer. When the newest operation has failed, the form shows why
above the inputs, including per-node reasons such as an SSH address outside
the enrollment profile's `allowedGatewayCIDRs` / `allowedWorkerCIDRs`
(`SSHAddressDenied`) or an old record still held by work (`ReplacementBlocked`).
The SSH address column takes the endpoint A can reach (for example a
platform-mapped address), not the machine's internal private IP. The deploy drawer opens even when enrollment is disabled so
the administrator notice is visible; submission stays disabled. Narrow screens
stack the charts;
wide tables keep their own horizontal scroll region.

Nodes appear in a summary table with private IP, native Node state, container
Agent readiness, Agent version, schedulability and heartbeat age. Expand a row
to inspect identity, native addresses and node information, environment,
network scope, heartbeat timestamp, capacities, authorized GPUs, capabilities
and conditions. Details are collapsed by default. Native Kubernetes Node phase
and container Agent readiness remain separate: a native Node may be Online
while its container Agent is stale. A stale Agent remains unschedulable
regardless of native connectivity.

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
4. Start the extension controller, Agent API, and the updated RLark Gateway/UI. Enable
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


## Build, enable and debug in RLark

The former standalone console binary, login and UI are retired; management is
now served by the main RLark UI. Rebuild both Gateway
and the main UI; restarting an old UI image cannot add the page. Extension
runtime/controller binaries retain their independent build entry point.

```bash
make -C apps/rlark gateway
make -C extensions/container-runtime build
npm --prefix apps/rlark-ui ci
npm --prefix apps/rlark-ui run build
kubectl apply -f extensions/container-runtime/config/crd/bases/
kubectl apply -f extensions/container-runtime/config/rbac/container-runtime-management.yaml
```

Add `--enable-container-runtime=true` to the existing Gateway startup arguments,
retaining its management API, authentication and other deployment configuration.
For optional node registration, also apply
`container-runtime-management-registration.yaml` from the same RBAC directory
and add `--container-runtime-enable-node-registration=true`. For Raw Job
submission apply `container-runtime-management-jobs.yaml` and add
`--container-runtime-enable-job-submission=true`. Remove the respective additive
binding when disabling a capability. Extension/controller/Agent execution flags
and RBAC are still separately required to actually execute work.

For diagnostics, co-locate the Agent API loopback listener with Gateway in the
same network namespace (for Kubernetes, the same Pod), then configure Gateway
with `--container-runtime-diagnostics-origin=http://127.0.0.1:9446`. Keep
`--log-read-addr=127.0.0.1:9446` and `--gateway-jwt-key-file=...` on Agent API.
Only literal loopback HTTP origins are accepted. No diagnostics origin means
the diagnostics handlers return unavailable; inventory management still works.

Deploy the new Gateway binary/image and main UI static build/image using the
existing deployment process. For local UI debugging, run
`npm --prefix apps/rlark-ui run dev` with the existing Vite Gateway proxy
configuration. Sign in through RLark as an administrator and open
`/admin/container-clusters`. Cluster links include namespace/name, so refresh
and browser back/forward preserve selection. The entry is visible even when
the extension is disabled: requests then return 503 and the page shows an error,
not empty/zero inventory. Missing CRDs or insufficient API permissions likewise
remain visible failures. Options requiring the extension are rejected at
Gateway startup when `--enable-container-runtime` is absent.

The shared UI API layer handles token storage, expiry and logout. An extension
401 clears the same RLark session; there is no second login or browser token.

### Module boundary and upstream updates

- `core/` contains companion API types, provisioning/submission, typed
  management handlers, artifact validation and shared contracts. It depends on
  the native API module, not the native application or Docker SDK.
- `extensions/container-runtime/` contains Agents, runtime providers,
  controllers and mTLS transport. It depends on `core/` and native execution
  interfaces; Gateway imports only `core/`, avoiding a Go module cycle.
- `apps/rlark/pkg/gateway/container_runtime.go` is the integration adapter.
  Existing native edits are configuration, handler initialization and explicit
  route registration; UI edits are navigation and route mounting. Shared
  ownership/tenant metadata is canonical in `api/metadata` with existing native
  exports preserved as compatibility aliases.
- Root workspace and Gateway Docker build include `core/`; extension
  `make test`, `make vet` and CRD generation enter both module roots explicitly.
  CRD group/version and wire contracts remain unchanged by the extraction.

### Main UI integration validation

Repository `make test-go`, extension test/vet/build, Gateway build, all 150 main UI
tests, the UI production build and strict English/Chinese documentation build
passed. Browser smoke with mocked API transport covered the existing Admin shell,
two-cluster selection, reload/back/forward, English/dark theme, disabled-extension
503 and shared-session logout on 401, without JavaScript exceptions. This does
not establish a live cloud deployment or Docker GPU acceptance.

Core-module lint and incremental native lint pass. Full native lint reports 30
pre-existing findings; the runtime module reports 30 pre-existing findings.
Frontend lint exits successfully with 58 existing warnings. Changed UI files pass
formatting; five unrelated existing test files fail a broader formatting check.

The preview UI creates Raw or Docker clusters. API/CLI provisioning and the
administrator console can register Docker hosts and accept [host observations or
opt-in single-container execution](docker-agent-observation.md). Docker Job
submission remains unsupported in this form. Package export, artifact publication and node drain remain CLI/operator workflows;
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

The replacement package was subsequently installed in separate private
directories and its temporary reverse tunnel restored. The current Agent reports
`preview-4b8258c`, Ready and schedulable with one authorized GPU. The real
`TestCloudRawLifecycle` rerun passed success, GPU reuse, exit-code-7 failure and
suspension in 165.10 seconds; its Tasks, bindings and allocations were removed
normally. This remains single-node Raw acceptance, with no Docker socket or
multi-node Ray/RLinf validation.


## Bounded Raw log preview

The console can show a retained Raw Task snapshot when the API is started with
`--log-read-addr=127.0.0.1:9446` and
`--gateway-jwt-key-file=/private/gateway-jwt.key`. The Agent package must be
exported with `enableExecution: true` and `enableLogReads: true`; the Agent
polls the existing mTLS endpoint and the supervisor reads only the exact retained
Task log. `--log-read-addr` accepts an explicit loopback address only. Gateway uses `--container-runtime-diagnostics-origin=http://127.0.0.1:9446` to proxy the fixed
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
plain text; historical archives, follow/tail and terminal/exec are not part of this preview.
Docker diagnostics are available separately when the node advertises both
`docker-diagnostics-v1` and `docker-container-execution-v1`; the stats route returns
one bounded Linux sample and does not provide GPU utilization or CPU percentages.

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
and an explicit `--container-runtime-enable-job-submission` flag; the default remains disabled. Apply
`extensions/container-runtime/config/rbac/container-runtime-management-jobs.yaml`
alongside the base management role, then add the flag to the existing Gateway
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
Enable `--container-runtime-enable-node-registration=true` and install the additive
`extensions/container-runtime/config/rbac/container-runtime-management-registration.yaml`.
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

## Docker diagnostic reads (preview)

Docker allocation rows now provide manual logs and resource snapshots when the
node advertises `docker-diagnostics-v1` and `docker-container-execution-v1` and
cleanup is not confirmed. The `/api/v1/container-runtime/stats` GET route uses the
same four exact identity parameters and Gateway administrator JWT as logs. The
fixed loopback proxy uses the existing `--container-runtime-diagnostics-origin`. No new browser
credential, arbitrary upstream route or remote Docker socket is exposed.

Snapshots show sample time, cumulative CPU nanoseconds, memory usage/limit, PIDs,
and block read/write bytes. uint64 values stay decimal strings; missing data is
unavailable. They do not imply CPU percentages, GPU utilization or attributed
host-network traffic. Only manual reads are provided. Selection/logout and
observed allocation replacement/cleanup or capability revocation cancel pending
reads and clear displayed data. Reads are transient, bounded and revalidated at
return; API restart requires a manual retry. See [Docker Agent](docker-agent-observation.md)
for enablement, ownership, limits and failure/recovery. The underlying Engine
transport is tested with a simulated daemon; real Docker GPU acceptance remains
outstanding. Browser Docker Job submission remains separate work; Docker node
registration and idle Agent upgrades are covered by the additive management plan
in [Docker cluster management](docker-cluster-management-plan.md).


## Raw Notebook resources in native Nodes

Raw existing-container nodes now project their authenticated resource observations
into the existing RLark `Node.status.capacity`, `allocatable`, `used`, `nodeInfo`
and `addresses` fields. The detail DTO carries the native generation so both views
reject stale resource/scheduling conditions. The native Node page and Container Cluster detail reuse
one read-only Node overview. Kubernetes Pod request fallback is never used for Raw.
The Kubernetes workload placement picker excludes explicit Raw and Docker nodes;
resource visibility does not add Kubernetes workload support to those runtimes.

- CPU is bounded by effective process affinity and the smallest cgroup v2 ancestor
  quota. Memory uses the smallest finite cgroup ancestor limit; an unlimited or
  unavailable memory limit is omitted, not replaced with physical host memory.
- GPU capacity counts only the operator-authorized UUID set verified visible by
  the Agent. Extra visible devices do not grant scheduling authority.
- `Allocatable = Capacity`: these are container resource bounds. No arbitrary
  Agent/system overhead is subtracted. Zero is distinct from missing information.
- `Used` is RLark workload reservation accounting, **not live CPU/GPU/memory
  utilization**. The current Raw runtime reserves the entire Notebook per Task,
  regardless of requested GPU count. CPU, memory and authorized GPUs remain fully
  reserved while its exact allocation exists, including terminal Tasks, deletion,
  cleanup confirmation, lost heartbeat and restart. Only allocation removal frees
  that reservation. Upgrade holds and external supervisor occupancy block
  scheduling separately and do not count as RLark workload Used.
- The control plane stamps `ContainerNode.status.resourceObservedAt` with server
  time when a valid positive CPU observation arrives. Agents may optionally report
  OS, architecture and kernel; older Agents without these fields remain compatible
  but do not get fabricated system information. GPU-only legacy heartbeats do not
  become a complete fresh resource observation.
- The independent resource reconciler re-reads bindings and allocations, periodically
  repairs every 15 seconds, and owns only the projected resource/system fields,
  `RawResourcesReady` / `RawSchedulingReady` conditions, and
  `containers.rlark.io/native-resource-projection` / `resources-observed-at`
  annotations. Projection ownership is fenced by the companion UID. Core retains
  connectivity `Ready`/Phase and the Node Spec. Status patches use optimistic locks
  to preserve concurrent conditions. No resource projection acquires/releases a
  scheduling lock or renews a heartbeat.
- Observations older than 45 seconds, future timestamps, revoked credentials,
  stale Spec observations, ambiguous ownership and failed allocation inventory
  reads cannot advertise free resources. Last known capacity/reservations can stay
  visible with an unavailable/stale state; the UI shows unknown availability as
  `—`. A known cordon, maintenance hold or busy Agent shows zero dispatchable
  resources even when resource accounting is zero. Native resource views are
  eventually reconciled observations; the current allocation checks remain the
  authoritative dispatch fence.

This preview does not report physical-host capacity, live utilization, GPU models,
storage quota, disk pressure or hardware topology. A missing disk field means
unreported, not a healthy or empty disk. Notebook nodes require neither a Docker
socket nor systemd / `CAP_NET_ADMIN` for this resource view.

### Resource rollout and read-only verification

1. Apply the updated `containers.rlark.io_containernodes.yaml` CRD before the new
   Agent API; then deploy the extension controller, Agent API and native Gateway.
2. Deploy the native Node controller with optimistic status patches, and rebuild
   the main RLark UI. The two views share the native resource snapshot; there is
   no separate dashboard or independent resource database.
3. Upgrade only the Notebook Agent to collect optional system information. Keep
   the Node cordoned during acceptance and do not restart an active supervisor.
4. Check the exact companion/native Node UIDs, server observation timestamp,
   authorized GPUs, resource conditions and `status.nodeInfo`. Verify a second
   observation refreshes the timestamp without changing ownership or cordon.

```bash
kubectl --kubeconfig "$MGMT_KUBECONFIG" -n ksyun-container-agents \
  get containernodes.containers.rlark.io ksyun-a -o yaml
kubectl --kubeconfig "$MGMT_KUBECONFIG" -n ksyun-container-agents \
  get nodes.rlinf.io ksyun-a -o yaml
```

A node without a fresh projection remains connected if its heartbeat is valid,
but resource availability stays unknown. Upgrading this view neither starts a
workload nor releases an allocation; task execution acceptance is separate.


### Read-only cloud acceptance on 2026-11-03

The existing `ksyun-a` Notebook was verified through the real management API,
Gateway and main UI (native Node and expanded Container Cluster detail). Both
views matched the canonical Node snapshot: 100 CPU cores, 1,270,236,577,792 bytes
of memory (about 1183 GiB), five authorized GPUs, Linux/amd64, kernel
`5.15.0-88-generic`, and InternalIP `10.1.3.100`. These are this Notebook's
reported bounds, not the earlier one-GPU test machine's specification.

Capacity equaled Allocatable; RLark workload Used was zero and dispatchable
availability was zero because the Node stayed cordoned. The exact native and
companion UIDs were unchanged, connectivity stayed Online, and observation times
advanced across heartbeats. Only the Agent collection process restarted; the
existing supervisor and Raylet PIDs/start times were unchanged. No workload was
submitted. Both actual browser views reported no JavaScript exceptions.

The local Gateway startup configuration now persists `--enable-container-runtime`
and the optional registration/submission flags; keeping these only in a temporary
process would make the console return 503 after restart. Diagnostic log/stats
proxying remains separately configured and is not part of this acceptance.
