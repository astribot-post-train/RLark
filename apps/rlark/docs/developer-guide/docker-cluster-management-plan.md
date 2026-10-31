# Docker cluster management delivery plan

This plan tracks the operator-facing path for adding fixed private Docker GPU
hosts as a separate RLark cluster. The implementation stays in
`extensions/container-runtime`; native Cluster, Node, Job and Task identity is
reused, but Kubernetes and Docker clusters remain separate through the
companion `runtime` discriminator.

## Current contract

- A Docker cluster is created with `runtime: Docker` and its own Agent namespace.
- Each registered host is one companion ContainerNode and one native Node with
  `AgentTypeDocker`.
- The Agent runs as root on the host and talks to the host Docker Unix socket;
  Docker-in-Docker and mounting the socket into workloads are unsupported.
- The scheduler reports private address, network scope, authorized whole-GPU
  inventory, Engine identity, heartbeat, capabilities, busy state and held
  allocations. A node is schedulable only when all current identity and
  ownership checks pass.
- Agent release rollout is a durable Cluster target. The controller selects one
  fresh idle node at a time, creates an exact upgrade allocation, and waits for
  the Agent journal and cleanup proof before selecting another node.

## Delivery increments

1. **Create the right cluster.** The console creation form must choose Raw or
   Docker and retain that choice in the idempotent operation request. The API
   already validates both values; the UI must no longer silently create Raw.
2. **Join Docker hosts.** Docker cluster detail must expose the existing
   administrator node-registration flow. The server derives the runtime from
   the bound cluster and creates a matching Docker native Node/ContainerNode;
   it never accepts a client-supplied runtime or GPU entitlement.
3. **Distribute Agent updates.** Docker packages may opt into the same
   root-owned updater used by Raw packages. Upgrade admission must verify the
   matching Docker native Agent type, runtime, Engine identity and idle whole-
   host allocation. A Docker node is upgraded only through its exact package
   paths and durable updater journal.
4. **Make the page useful.** The detail view shows runtime, node inventory,
   Engine/capability observations, GPU count, occupancy, allocations, target
   release and upgrade phase. Unsupported workload actions remain hidden for a
   Docker cluster; Docker Ray Job submission is a separate preview gate.

## Safety and recovery requirements

- Reconcile from current objects; duplicate or lost requests resume only when
  operation UUID, object UID, request fingerprint and owner references match.
- A stale heartbeat, replaced instance, changed credential, changed release or
  held allocation fences both scheduling and upgrade. Cordon blocks new workload
  starts; it does not pause independent idle Agent rollouts.
- A response loss after package creation or upgrade claim retains the operation
  and exact lock; it must not create a second Agent or release an uncertain
  allocation.
- Rollout is idle and serial by default. It does not stop running Docker jobs,
  revoke an already authorized start, or claim kernel-level port reservations.
- UI filtering is advisory. Gateway/API and direct management paths perform the
  tenant, ownership, runtime and authorization checks.

## Acceptance gates

- Unit and failure/recovery tests cover Docker cluster creation, Docker node
  registration, package validation, upgrade admission and controller restart.
- English and Chinese documentation, CRD output and the extension UI build pass.
- A real Docker Engine host must complete Agent observation, one-container
  lifecycle, exact cleanup, and one idle Agent update before Docker rollout is
  called cloud-accepted. The current Kingsoft Cloud notebook is not that host:
  it has no Docker socket and remains Raw/Agent-only validation infrastructure.

This plan is deliberately additive. It does not add Docker-specific fields to
the native RLark API or claim production Kubernetes parity.
