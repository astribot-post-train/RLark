# Station API types (experimental)

This standalone Go module defines the experimental `station.rlark.io/v1alpha1` API types, generated CRDs, controller prototypes, and an executable controller manager. The manager has example RBAC and Deployment manifests but is not deployed. The Station API executable is in `apps/rlark/cmd/station-api`, and the prototype agent is in `apps/rlark/cmd/station-agent`; both are not deployed. The agent reports kernel boot time from `/proc/stat` along with its heartbeat. WireGuard, hardware and probe collection, production UI integration and deployment are not implemented, and generated CRDs are not installed in production.

See the [API contract](../../docs/design/station-api-contract.md) for field ownership, namespace mapping, validation, lifecycle semantics, and remaining design questions.

The identity coordinator rejects cancelled acquisitions and releases the gate if cancellation races with acquisition. This gate only serializes the active manager process. Graceful handoff of cooperative leader-only probes has been tested against isolated kcp; distributed fencing and failover during reconciliation remain unverified.

Run tests and vet with the repository workspace disabled:

```bash
GOWORK=off go test ./...
GOWORK=off go vet ./...
```

`TestLiveStationReconcileAndRebind` additionally requires `STATION_TEST_ISOLATED_KUBECONFIG` pointing to a disposable workspace with all five CRDs installed. It creates a namespace and resources and must not run against a production workspace. It uses test doubles for cluster registration and the member Node reader and tests recovery from an injected audit failure against real CRD status and audit resources.

Regenerate the checked-in deepcopy code and CRDs with the pinned generator:

```bash
GOWORK=off ../../api/bin/controller-gen-v0.18.0 object paths=./api/v1alpha1/...
GOWORK=off ../../api/bin/controller-gen-v0.18.0 crd:crdVersions=v1 paths=./api/v1alpha1/... output:crd:artifacts:config=./config/crd/bases
```
