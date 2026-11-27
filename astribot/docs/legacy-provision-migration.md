# Migrate historical SSH hosts and cluster provisions

## Reuse a historical worker

Retry SSH join on the original host. Gateway requires the pinned SSH fingerprint and the matching WireGuard public key. For a worker installed before the marker stored the cluster and node name, Gateway compares the node name from the original installer journal and a fingerprint of the worker's API server and CA against the selected member's native cluster information. The member API must return a native Node NotFound for that name. The host checks the proof again under its local lock before resetting the worker installation or changing the tunnel.

An active installer, a control plane, a current same-named Node, a different API endpoint or CA, or missing evidence stops automatic recovery. Keep the old record and resolve its ownership before trying again.

## Review and record historical cluster ownership

In the administrator console, open **New Cluster**, select a record under **Creation history**, and choose **Review historical ownership**. This action is available for legacy records. The console reads the current Cluster, SiteNetwork and relevant credential metadata. It shows each candidate's status, UID and creation time when available. Compare these with independent installation, audit or backup records. Select only resources whose original ownership you can verify, enter the evidence reference, and confirm. If a live Cluster still exists, you must also review it unless its original ownership is already recorded. Partial SiteNetwork declarations can be selected when the remaining entries agree with the original provision.

For a historical control-plane host, open **Review original control-plane host**, enter its SSH password and choose **Read host identity**. Gateway connects using the recorded host fingerprint and reads the native `kube-system` UID. Compare the returned UID with the original cluster evidence and confirm it separately. The console clears the password after the read. If the host or its native API is unavailable, you can still review the management resources.

The console submits the observed UIDs and resource versions for you. If a selected object changes, reload and verify the candidates again. Confirmation writes only ownership metadata and audits the evidence reference. It does not clear the provision, reset a host, expose Secret data or delete independent node records. Use the existing cleanup action after confirmation.

The administrator API exposes `GET /api/v1/cluster-provisions/{name}/migration-candidates`, `POST /api/v1/cluster-provisions/{name}/migration-candidates/host`, and `POST /api/v1/cluster-provisions/{name}/migrate-ownership`. The existing programmatic confirmation request remains supported. A record whose original ownership cannot be verified should be retained.

Temporary network and upstream service errors are retried before rejecting a recovered SSH Node UID. A different live UID is still rejected immediately.
