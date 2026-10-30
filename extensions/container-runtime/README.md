# RLark container-runtime extension

This is an independently buildable Go module for moving Docker and Raw runtime implementation out of the native RLark module. Its API uses the `containers.rlark.io/v1alpha1` group and companion resources pinned to native object UIDs. It reuses RLark's exported execution contracts; native RLark remains a versioned dependency and contains only generic ownership dispatch.

Current contents are an extraction copy of preview Docker/Raw runtime, allocation and supervisor packages, typed companion API/CRDs, UID/tenant binding checks, and a validation-only controller. It verifies native Cluster, Node and Job object UIDs and tenant/runtime bindings; it deliberately does not schedule work, mark Agent registration ready, publish artifacts, or permit deletion while companion nodes remain. Native Jobs that declare `rlark.io/controller-owner: containers.rlark.io` fail closed if the extension owner is not installed. A production Agent enrollment/heartbeat API, allocation and cleanup reconciliation, release hosting/rollout, UI and safe live-data migration remain incomplete. Do not deploy this module as a production scheduler.

Run `make test`, `make vet`, `make build`, or `make generate-crds` here. `GOWORK=off` is used to prove the module resolves from its own `go.mod` and `go.sum`.

The detailed target, rollout, cloud test constraints and acceptance gates are maintained in the paired RLark developer plan at `../../apps/rlark/docs/developer-guide/raw-container-runtime-plan.md` and its Chinese counterpart.
