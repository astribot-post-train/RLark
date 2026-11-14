# Moving a Cluster to Declared Node Agents

Rehearsed 2026-11-14 on the local `rs-farm` cluster (three nodes, live-shaped per-node Deployments, then an rlarkadm-shaped DaemonSet). Not yet run on a live cluster. Feature: [Node Agents Declared on the Cluster](../../apps/rlark/docs/admin-guide/agent.md#node-agents-declared-on-the-cluster). [中文](node-agents-migration.zh-CN.md)

This is a one-time batch step per cluster. RLark itself never deletes the old node-agent workloads: they were made by hand or by rlarkadm, and only the batch tool removes them.

## Facts the steps rely on (measured)

- Two node Agents on one node do not conflict. The Server rejects a second tunnel for a node key that already has one five times (`websocket: bad handshake` in the new agent's log, one try every 4–6 s), then accepts it; both sessions then live side by side. The node-heartbeat Lease stays with the first connection; the node-server socket moves to the newest agent through its stable symlink. Measured: tunnel accepted 25–31 s after the new Pod started; no management Node went Offline while both ran.
- A DaemonSet Pod is Ready when its node server is up, before its tunnel is accepted. Wait for the tunnel (below), not only for Ready.
- Removing the old agent costs what any node-agent restart costs: the Lease is not released when a tunnel closes, so it expires 45 s after the last renewal and the other connection takes it at its next 15 s try. Measured per node: Offline 0, 5, 25 s (hand-made Deployments), 0–10 s (DaemonSet handed over). Running Tasks are not touched.
- Rolling the Server or the cluster Agent has its own heartbeat gap (Cluster Offline up to about 1 min; every Node 0–45 s when the Server restarts). Do those steps apart from the node-agent steps.

## Steps for every cluster

1. Management plane first: apply the new `clusters.rlinf.io` CRD, then roll Server, Gateway and controller-manager to the release. The Server grants each cluster Agent read access to its own Cluster when it connects; a new cluster Agent against an old Server cannot start its node-agent controller.
2. Check every Node that is to run a node Agent: `kubectl get nodes -o custom-columns=NAME:.metadata.name,SOCKET:.metadata.annotations.kubeadm\.alpha\.kubernetes\.io/cri-socket,MANAGED:.metadata.labels.rlark\.io/enabled`. The socket must lie under `/run` (a Node without it runs its node Agent without image pre-pull), and `rlark.io/enabled=true` must be set. Check that the cluster Agent's image can be pulled under the same name on every one of those Nodes: one DaemonSet has one image.
3. Cluster Agent Deployment (`rlark-agent`), one edit: `strategy: {type: Recreate}`; image = the release; `--image=<the same image>`; `POD_NAME` and `POD_NAMESPACE` from the downward API (rlarkadm sets them); `--mode=cluster` (an agent in `--mode=both` also runs node functions on its own node, without the `/host/run` mount, so it has no image pre-pull there; the DaemonSet takes that over). Wait until the Cluster is Online again.
4. Hand over what already carries the name `rlark-agent-node` in the agent's namespace (only what exists): `kubectl -n rlark-system label sa/rlark-agent-node ds/rlark-agent-node app.kubernetes.io/managed-by=rlark-agent` and `kubectl label clusterrole/rlark-agent-node clusterrolebinding/rlark-agent-node app.kubernetes.io/managed-by=rlark-agent`. Do this before step 5: the cluster Agent refuses unlabelled objects and retries with back-off, and labels alone do not wake it (measured 14 s after a 30 s refusal; it can be minutes).
5. One Gateway request (administrator): `PUT /api/v1/clusters/<cluster-id>/node-agents` with body `{"enabled": true}`. `<cluster-id>` is the Cluster object name (`rlark-<agent id>`). Measured: DaemonSet Ready on three nodes 2 s later.
6. Per old workload, oldest first: wait until the new Pod on each of its nodes has had no `tunnel connection error` for 15 s (`kubectl -n rlark-system logs <pod> --since=20s | grep -c 'tunnel connection error'` is 0 and the Pod is older than 40 s), then delete the old workload. Next workload after the management Node is Online again with a new Lease holder (`kubectl -n <cluster-id> get lease node-heartbeat-<node>`).
7. Verify: `kubectl -n rlark-system get ds rlark-agent-node` desired = ready; management Nodes Online; one Job per node pulls (its management Node shows `status.pullProgress`).

## Robot cluster (`alexfu` = 3090 control plane, `s1u-01-x86`, `s1u-02-x86`)

Today: one hand-made Deployment per node, pinned by hostname with its own `--containerd-socket`: `rlark-node-agent` (s1u-01-x86), `rlark-node-agent-alexfu`, `rlark-node-agent-s1u-02`; the cluster Agent `rlark-agent` runs `--mode=both`. Expected sockets: `unix:///run/containerd-fleet/containerd.sock` on `alexfu` (installer runtime), `unix:///run/containerd-robot/containerd.sock` on the robots (check in step 2).

Steps 1–5 as above (step 3 also switches `rlark-agent` to `--mode=cluster`; step 4 only if `kubectl -n rlark-system get sa,ds rlark-agent-node` finds something). Step 6 node by node, a robot without a running Run first: delete `deploy/rlark-node-agent-s1u-02`, then `deploy/rlark-node-agent` (s1u-01-x86), then `deploy/rlark-node-agent-alexfu`. Each robot shows Offline in RLark for 0–25 s; a robot enrolled afterwards gets its node Agent from the DaemonSet (rehearsed: Ready 70 s after admission, no further step).

## Office cluster

Today: DaemonSets `rlark-local-node-agent` (pinned to `cailiang`, `/run/containerd/containerd.sock`, image name `localhost:5555/...`), `rlark-node-agent` and `rlark-node-agent-containerd` (`containerd-fleet` nodes). None is named `rlark-agent-node`, so step 4 is usually empty. Step 2 matters here: `cailiang` pulls the agent image under a different name than the other nodes; give its containerd the same registry name (registry trust) before step 5, or its DaemonSet Pod will not start. Step 6 per old DaemonSet: delete `ds/rlark-local-node-agent`, then `ds/rlark-node-agent-containerd`, then `ds/rlark-node-agent`, each after the new Pods on its nodes passed the tunnel check; all nodes of one old DaemonSet take their 0–25 s gap together.

## Cloud cluster

Today: rlarkadm's DaemonSet `rlark-agent-node`. Step 4 hands it over (with its ServiceAccount, ClusterRole, ClusterRoleBinding); step 5 then makes the cluster Agent rewrite it into the declared form, a rolling update one node at a time (rehearsed: 1 min 40 s for three nodes, Offline 0–10 s per node). Step 6 has nothing to delete.
