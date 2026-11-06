# Mixed Raw and Kubernetes Job preview

Status: controller-level preview with automated tests. Cloud-to-robot Fleet
acceptance, automatic relay injection and the unified UI submission flow are
not complete. Do not use an Agent heartbeat or these tests as evidence that
Lumo2 inference is ready.

## Cluster and Ray identities

A, B and C are members of one native RLark logical container Cluster. Each
Notebook runs a Raw Agent and supervisor in its existing container; Docker
Engine, privileged containers and a second Kubernetes installation are not
required. A additionally hosts the management gateway and, when configured,
the Ray TCP gateway. The robot x86 stays in its existing Kubernetes Cluster.

One cluster-scoped native Job owns the Raw rollout Task in the container
Cluster's Agent namespace and the Kubernetes Env Task in the robot Cluster's
Agent namespace. B does not join the robot's Kubernetes cluster. Both workload
roles must use the same Ray cluster/head. This preview never creates a Ray head;
the chosen head's lifecycle and Raw process command remain operator-managed.

## Explicit placement contract

Set the native Job annotation `rlark.io/controller-owner: containers.rlark.io`.
With the extension controller's `--enable-job-reservations=true`, a namespaced
`containers.rlark.io/v1alpha1` ContainerJob pins the Job and the Raw Cluster by
name and Kubernetes UID. Its immutable `spec.kubernetesTargets` list binds each
Kubernetes template to an exact native Cluster instance:

```yaml
# Fragment of ContainerJob.spec; replace names and UIDs from current API reads.
runtime: Raw
cluster:
  name: ksyun-containers
  uid: REPLACE_CONTAINER_CLUSTER_UID
job:
  name: REPLACE_JOB_NAME
  uid: REPLACE_JOB_UID
tenantID: REPLACE_TENANT_ID
kubernetesTargets:
  - template: env
    cluster:
      name: rlark-robot-s1u-01
      uid: REPLACE_ROBOT_CLUSTER_UID
```

The binding namespace must be the Raw Cluster's authoritative
`spec.agentNamespace`. Targets have no namespace field value; the controller
resolves each Cluster's Agent namespace from current state. Missing, duplicate,
extra, replaced, cross-tenant, deleting, cordoned, draining and ambiguous targets
are rejected. A target namespace claimed by a container Cluster is rejected.
Scheduling and Raw dispatch revalidate this same placement contract; stale
Validated conditions never grant permission to start.

The mixed preview accepts 2–16 uniquely named templates with at least one Raw
process and one native Kubernetes Ray worker. Each Kubernetes template uses one
Deployment replica and an explicit unicast IPv4 `headAddress` (without port;
the native adapter uses 6379), `headTaskName`, `totalNodes` and a nonnegative
node rank. Multiple Kubernetes workers must agree on the head identity and
total node count and use distinct ranks. Head templates, Docker templates,
legacy bindings, per-template suspension and environment add-ons are rejected.
The Raw command must be configured consistently with that head; this is not
inferred from arbitrary process arguments.

Create the native Job suspended, read its assigned UID, create the exact binding,
then activate the Job with UID/resourceVersion preconditions. Choose B with a
Node selector matching its actual native Node labels; Raw allocation retains
whole-node GPU exclusivity. The existing UI Raw Job form does not submit this
mixed contract yet. Install the regenerated ContainerJob CRD before using the
field and update both the extension controller and execution API together.
Ray group plan members are a Kubernetes map list keyed by `rank`; regenerate
and install the matching CRDs rather than copying only the new target field.

## Result, stop and recovery

Raw runtime effects remain behind the separately enabled mTLS Task API and
Agent. The native Kubernetes Agent owns its Task runtime and cleanup finalizer;
the extension does not acquire a Raw allocation for Kubernetes Tasks. The Job
aggregates fresh per-runtime observations, not merely Task creation.

For a native worker's natural success/failure, the extension saves a bounded
Task-UID/generation result receipt in its own annotation, requests durable
`spec.suspended=true`, and waits for the native Host to report current-generation
Stopped after runtime cleanup. The original result is retained. Raw cleanup
requires the exact supervisor/allocation proof and allocation disappearance.
Terminal Tasks are retained so controller restart does not create another run.
The extension owns the result annotation and
`containers.rlark.io/kubernetes-task-protection` finalizer; operators must not
edit them as a way to acknowledge cleanup.

Job suspension, deletion, restart or changed execution identity retires the
whole previous Task set before replacement. Stop requests fan out so a blocked
Kubernetes cleanup does not starve Raw cleanup. The extension waits for the
native Host's finalizer to disappear before removing only its own protection,
then observes the child absent. Unknown finalizers remain untouched. Uncertain
cleanup blocks replacement and retains identities for administrator recovery.

Members currently start independently: gang reservation, Fleet startup barriers,
automatic sibling cancellation on failure, and same-head validation inside an
arbitrary Raw command are not implemented. A Running Job means at least one
member has reported Running; it does not mean all Ray members are ready. Native
Kubernetes execution still consumes its Agent namespace. Updated native Agents
validate the canonical `rlark.io/target-cluster-uid` annotation against the exact
Cluster UID, tenant and unique `spec.agentNamespace` owner before claims and
execution. Conflicting legacy annotations fail closed; unpinned historical Tasks
keep their legacy contract. Deletion retains the existing owned cleanup path.
Deploy the updated native Agent before using this target fence. Stop mixed Jobs
before retiring their target Cluster or reusing its namespace. These limits
prevent treating this preview as complete Fleet orchestration.

## SSH bootstrap and runtime traffic

RLark's SSH public key is authorized on A. A's bootstrap SSH public key is
authorized on B/C in root's `~/.ssh/authorized_keys`; its private key stays on A.
If authorization persists across a Notebook restart it need not be added again.
Pin and verify the SSH host identity when enrolling a replacement. Do not copy
private keys into UI forms, status, logs or workload processes.

SSH is for installation and upgrade, not inference transport. Agent API traffic
uses the management gateway on A's public 30000. The optional TLS/smux Ray data
gateway uses A's 30001; compute peers connect to it directly, without inference
payloads traversing the local RLark service. Peer provisioning and process-scoped
relay injection are still operator-managed and require real bidirectional
Ray/Gloo Channel acceptance. Peer JSON configuration optionally accepts
`localAddress`, an existing unicast source IP for the connection to A only.
An unavailable source address fails the connection and is retried; it never
falls back to another source. It does not alter Ray destinations or routes and
cannot bind an address outside the peer's network namespace. No HTTP proxy,
SSH forwarding, WireGuard, TUN or global route changes are required by the
relay preview. Preserve existing Running Jobs and Ray heads; Lumo2/S1U testing
is Passive only.

The native Job, Raw Cluster and each Kubernetes target Cluster must have the
same tenant identity. This is an RLark authorization boundary, not a Ray or
network requirement. Cross-tenant sharing is not supported by this preview.
Changing test resource ownership requires an explicit administrative migration
and reissuing tenant-bound node credentials; changing labels alone is insufficient.

## Cloud preparation evidence, 2026-11-03

The idle cloud test resources were administratively aligned with the robot's
existing tenant. The migration recorded exact object UIDs and resource versions,
backed up the objects, paused the affected controller/Agents, verified there were
no allocations or Jobs, and rotated node credentials before restarting B.
Robot ownership and cross-tenant validation were preserved. This was an
operator recovery, not a general tenant migration API; old registration requests
and certificates must not be reused after an ownership change.

A's existing management gateway now also serves the TLS/smux data listener on
30001 in the same process. A synthetic request completed a round trip through
A's public 30001 to B's private peer. The temporary probe process and its gateway
authorization were then retired. This verifies the deployed transport, not a
robot peer, Ray membership, a Gloo Channel or an inference request. An Agent
heartbeat and a listening TCP port are insufficient readiness evidence.

B retains its existing Lumo2 environment and uses a separate dependency directory
for Ray 2.59.0 and Fleet's missing HTTP dependencies. The cloud's package mirror
did not contain that Ray version; its wheel was downloaded from the official
Python package index on the administration machine and installed on B without
changing global proxy settings or the existing environment. The selected
checkpoint's step-30000 Hugging Face directory, processor, BAT checkpoint and
statistics paths passed existence validation; Fleet rollout/backend imports
passed. A bounded standalone GPU check then loaded that model successfully in
about 102 seconds, allocating about 9.4 GB of GPU memory. The check exited and
released the model after the model check released its resources. A subsequent bounded
standalone prediction used a synthetic 29-element state and three synthetic RGB
images with the same checkpoint. It completed in about 200 seconds and returned
a finite `32 x 29` action array. This verifies cloud-side model prediction only:
no robot was connected, no actions were sent, and execution through an RLark Job
has not been verified.

After the platform stopped and restarted B's Notebook, its private address changed
to `10.1.3.201` and its process-namespace instance identity changed. The old Agent
correctly rejected the replacement instance. After checking that no Job, Task,
allocation or upgrade referenced the idle node, the administrator retired the old
native/companion Node instances and registered the replacement into the same
logical Cluster with fresh credentials. B's Agent and Raw supervisor are running;
its native Node is Online and remains cordoned. The B transport peer has an
established connection to A's private `10.1.3.189:30001` listener. This connection
does not prove that a robot peer has joined or that Ray/Gloo traffic passes.

A separate bounded B-only Ray 2.59.0 check successfully started a local head,
ran a GPU worker process, observed `CUDA_VISIBLE_DEVICES=0`, and verified that
the worker inherited the process-scoped relay shim. It completed in about six
seconds and shut down its Ray instance. This does not test a remote member or
the relay data path. Use a short Ray temporary root such as
`/tmp/rlk-ray-b-v2`: putting the session under a long deployment directory caused
startup to fail because Linux Unix-domain socket paths exceed 107 bytes. Ray
2.59.0 enables token authentication by default; provision the same protected
token to the head and joining worker rather than assuming their independently
generated tokens match. Never publish it in a Job manifest, logs or documentation.

At the last check the robot's Orin and x86 addresses were unreachable from the
RLark service machine and the robot Cluster was Offline. No mixed Fleet Job
has been activated or accepted as ready. B remained cordoned at that historical check. Restore robot reachability, verify current addresses,
align Python/Ray and RLinf dependencies, validate the deployed Kubernetes
Agent's explicit-head contract, and complete bidirectional Ray/Gloo and Passive
inference acceptance before declaring this path ready. The dedicated transport
credentials in this deployment expire on 2026-11-10; renewal is manual and
requires updating pins and restarting the affected transport processes.

See [gateway plan](container-gateway-plan.md) and
[Raw runtime plan](raw-container-runtime-plan.md) for transport evidence and
remaining acceptance gates.
