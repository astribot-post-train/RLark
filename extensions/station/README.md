# Station API types (experimental)

`NodeOperation` is an experimental API for worker lifecycle operations. The
manager registers its reconciler only with `--enable-node-operations`; the
default deployment leaves it disabled. Gateway create/get/list routes are
administrator-only. The Stations UI can submit Join and Remove operations and
resume status polling from the operation history. Join selects the standard
containerd or Fleet socket profile, verifies the chosen socket before kubeadm,
and applies matching Node labels only after checking the new Node UID. The member
cluster must already have a matching node-agent DaemonSet. If a reused Node name
has a Station bound to an older UID, kubeadm may complete while RLark readiness
waits; after a fresh verified observation, submit an explicit Rebind and the
Join reconciler resumes. Offline Stations can be
reviewed for rebind when such an observation exists. The Join path is wired to an
experimental SSH executor and requires the CRD, member credentials, dedicated
RBAC and RLark readiness integration. Removal scans every registered member
cluster, drains, removes UID-owned WireGuard peers/config, verifies that no
references remain, waits for the network DaemonSet Pod to stop, resets the
worker and deletes the exact Node UID. All member kubeconfigs and network
ConfigMaps must be reachable. Unmarked legacy configuration blocks before
drain unless a fresh Node-UID-bound Station observation proves its key/address.
Do not enable this on production clusters. Internal
helpers derive kubeadm discovery information from the
verified member kubeconfig and create or revoke an operation-owned short-lived
bootstrap token. The SSH helper requires a pinned host key; the drain helper refuses
control-plane nodes, unmanaged Pods and Pods with `emptyDir`, and uses the
Kubernetes eviction API. A 2026-11-12 local-development Notebook test completed
Join and Remove, including kubeadm reset and exact-UID deletion. It required
manual Agent setup for Join and manual removal of node-pinned test Deployments
before Drain could finish; it did not verify WireGuard enrollment or a clean,
disposable cluster. The end-to-end lifecycle remains experimental.

The standard containerd node-agent profile example is
`apps/rlark/docs/examples/agent-node-containerd-daemonset.yaml`; deploy one
matching profile per runtime in each member cluster. The opt-in controller patch is `deploy/controller-node-operations-patch.yaml`;
it only enables the manager flag. Apply the management and member RBAC manifests
separately and install the CRD first. SSH credentials are stored in the
dedicated `station-operation-credentials` namespace, outside the ClusterRole
resource permissions; the controller can read/delete Secrets only in that
namespace. Ensure the Gateway can create there before accepting operations.
The patch is not a production deployment recipe and must not be applied to a
cluster with workloads before a disposable-cluster end-to-end test.

Install `deploy/station-agent-identity.yaml` once in member clusters that need
Station observations from non-WireGuard nodes. It excludes nodes labeled
`station.rlark.io/network=enabled`, which use the identity-plus-WireGuard
DaemonSet, and references the cluster's existing Station ConfigMap,
ServiceAccount and CA Secret.

The internal join helper validates every kubeadm argument and refuses an
existing kubelet configuration by default. An explicit reset option first
checks the requested host name, verifies that the target cluster contains
neither the Node name nor the host system UUID, refuses control-plane hosts,
and then runs one operation-marked `kubeadm reset`; an interrupted reset is
not replayed automatically. A failed kubeadm join also keeps its marker and
requires an operator to inspect the host before any explicit recovery. SSH sudo credentials are supplied separately
from SSH authentication through stdin. A kubelet client-certificate verifier
checks the member CA and node identity, but it does not establish ownership of
a Node UID or authorize adopting an existing node by itself.
The experimental reconciler waits for an operation-owned kubelet certificate
and Node registration, and uses optimistic status updates. Join and Remove were
also exercised against the local development cluster; the test did not establish
automatic Agent installation or safe removal of arbitrary workloads pinned to a
node.
Join attempts use a node-local operation marker to prevent replay after an
unknown outcome. The experimental SSH join driver verifies that the observed
kubelet certificate matches an approved CSR created by the operation's
bootstrap identity before observing the member Node and revoking the token.
This path remains experimental: the local test does not validate a clean,
disposable cluster, production authorization, or automated Agent provisioning.
The internal reconciler uses a finalizer to revoke operation-owned bootstrap
and SSH credentials on deletion. Deleting an operation does not undo a join,
uncordon a node, or reverse a partially completed removal; operators must
inspect the node before starting a replacement operation.
The experimental RLark verification stage enables node reporting only after
matching the member Node UID, then requires a matching Station binding and
ready RLark Node. An existing conflicting enablement label blocks the stage.
The short-lived bootstrap token is passed as a kubeadm argument and may be
visible to privileged users on the target machine while kubeadm runs.

This standalone Go module defines the experimental `station.rlark.io/v1alpha1` API types, generated CRDs, controller prototypes, and controller manager. A local deployment currently runs Station API, the identity controller/agent and the network DaemonSet; this does not make the extension production-ready. The NodeOperation CRD is installed in the KCP root workspace, and its opt-in controller completed a live local-development Notebook Join/Remove test. The test required manual Agent setup and removal of node-pinned test Deployments, and did not verify WireGuard enrollment or clean-host onboarding. The identity agent reports kernel boot time and the network gateway's public interface key/address snapshot. Hardware and probe collection, automatic WireGuard address/peer allocation, production authorization and recovery remain incomplete.
> **Naming (2026-11-12)**: the object in this directory is called a **Site Node** in the UI and in the external API: one machine's long-lived identity and network membership in the site. "Station" is reserved for a station of a Fleet Run, as the paper defines it. The CRD group `station.rlark.io`, the `Station*` kinds and the binary names are kept as code names; the migration and the enrollment design are in [Station v2](../../docs/design/station-v2.zh-CN.md).

This standalone Go module defines the experimental `station.rlark.io/v1alpha1` API types, generated CRDs, controller prototypes, and an executable controller manager. The manager has example RBAC and Deployment manifests but is not deployed. The Station API executable is in `apps/rlark/cmd/station-api`, and the prototype agent is in `apps/rlark/cmd/station-agent`; both are not deployed. The agent reports kernel boot time from `/proc/stat` along with its heartbeat. WireGuard, hardware and probe collection, production UI integration and deployment are not implemented, and generated CRDs are not installed in production.

See the [API contract](../../docs/design/station-api-contract.md) for field ownership, namespace mapping, validation, lifecycle semantics, and remaining design questions.

The identity coordinator rejects cancelled acquisitions and releases the gate if cancellation races with acquisition. This gate only serializes the active manager process. Graceful handoff of cooperative leader-only probes has been tested against isolated kcp; distributed fencing and failover during reconciliation remain unverified.

Run tests and vet with the repository workspace disabled:

```bash
GOWORK=off go test ./...
GOWORK=off go vet ./...
```

`TestLiveStationReconcileAndRebind` additionally requires `STATION_TEST_ISOLATED_KUBECONFIG` pointing to a disposable workspace with the five Station identity CRDs installed. It creates a namespace and resources and must not run against a production workspace. It uses test doubles for cluster registration and the member Node reader and tests recovery from an injected audit failure against real CRD status and audit resources; it does not test NodeOperation.

Regenerate the checked-in deepcopy code and CRDs with the pinned generator:

```bash
GOWORK=off ../../api/bin/controller-gen-v0.18.0 object paths=./api/v1alpha1/...
GOWORK=off ../../api/bin/controller-gen-v0.18.0 crd:crdVersions=v1 paths=./api/v1alpha1/... output:crd:artifacts:config=./config/crd/bases
```
