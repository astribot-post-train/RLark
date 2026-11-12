#!/usr/bin/env bash
# Move a node from the cluster it is in to another registered cluster, without
# logging in to it: an admission into the new cluster is written, and a Job in
# the old cluster runs the bootstrap image on the node, which leaves the old
# cluster and joins the new one (install.sh, step 0). The node keeps its
# container runtime and images, its WireGuard key, its tunnel address, listen
# port and uplink. The same command serves every node of every cluster.
#
#   migrate-node.sh OLD_KUBECONFIG NEW_KUBECONFIG NEW_CLUSTER_ID NODE API_SERVER STATION_API IMAGE [label=value ...]
#
# Environment: KCP (management kubeconfig), CA (Station API's CA file), TAINTS,
# PULL_SECRET, CONTROL_PLANE=yes to move the old cluster's control-plane node
# (its cluster ends there, so it is moved last).
# Afterwards the node's leftovers in the old cluster are removed: its Node
# object, its NetworkEnrollment, and its Station is retired.
set -euo pipefail
: "${KCP:=/home/cailiang/.local/share/rlark-runtime/kcp-admin.kubeconfig}" "${CA:=/tmp/station-api-ca.crt}"
OLD=$1 NEW=$2 CLUSTER=$3 NODE=$4 API_SERVER=$5 STATION_API=$6 IMAGE=$7; shift 7
HERE=$(cd "$(dirname "$0")" && pwd)
old() { kubectl --kubeconfig "$OLD" "$@"; }
new() { kubectl --kubeconfig "$NEW" "$@"; }
mgmt() { kubectl --kubeconfig "$KCP" "$@"; }
stamp() { echo "$(date +%T) $*"; }
started=$(date +%s)
pull=; [[ -z ${PULL_SECRET:-} ]] || pull="imagePullSecrets: [{name: $PULL_SECRET}]"

# What the node has now is what it keeps.
read -r old_namespace address port < <(mgmt get networkenrollments.station.rlark.io -A -o json | python3 -c '
import json, sys
for e in json.load(sys.stdin)["items"]:
    if e["metadata"]["name"] == sys.argv[1] and e["status"].get("phase") == "Enrolled":
        config = json.loads(e["status"]["config"])
        print(e["metadata"]["namespace"], e["status"]["tunnelAddress"], config["listenPort"])' "$NODE")
old_cluster=$(mgmt -n "$old_namespace" get networkenrollments.station.rlark.io "$NODE" -o jsonpath='{.spec.clusterID}')
uplink=$(mgmt -n station-system get sitenetwork default -o json | python3 -c '
import json, sys
for n in json.load(sys.stdin)["spec"]["nodes"]:
    if (n["clusterID"], n["nodeName"]) == tuple(sys.argv[1:3]) and n.get("uplink"):
        print(",".join(n["uplink"][k] for k in ("gateway", "source", "interface")))' "$old_cluster" "$NODE")
old get nodes -l node-role.kubernetes.io/control-plane -o name | grep -qx "node/$NODE" && mode=control-plane || mode=node
[[ $mode == node || ${CONTROL_PLANE:-} == yes ]] || { echo "$NODE is the control plane of $old_cluster: move the other nodes first, then this one with CONTROL_PLANE=yes" >&2; exit 1; }
stamp "$NODE: $old_cluster -> $CLUSTER, keeps $address port $port${uplink:+ uplink $uplink}"

command=$(KCP=$KCP ADDRESS=$address LISTEN_PORT=$port UPLINK=$uplink TAINTS=${TAINTS:-} "$HERE/admit-node.sh" "$CLUSTER" "$NODE" "$API_SERVER" "$STATION_API" "$@")
code=$(echo "$command" | sed -n 's#.*/b/\([a-z0-9]*\) .*#\1#p')
name=$(echo "node-migrate-$NODE" | cut -c1-63)
old -n kube-system delete job "$name" --ignore-not-found >/dev/null
old apply -f - >/dev/null <<JOB
apiVersion: batch/v1
kind: Job
metadata: {name: $name, namespace: kube-system}
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 86400
  template:
    spec:
      nodeName: $NODE
      hostNetwork: true
      hostPID: true
      restartPolicy: Never
      tolerations: [{operator: Exists}]
      $pull
      containers:
      - name: migrate
        image: $IMAGE
        imagePullPolicy: IfNotPresent
        securityContext: {privileged: true}
        env:
        - {name: RLARK_SERVER, value: "https://$STATION_API"}
        - {name: RLARK_CODE, value: "$code"}
        - {name: RLARK_MIGRATE, value: "$mode"}
        - name: RLARK_CA
          value: |
$(sed 's/^/            /' "$CA")
JOB
stamp "$NODE: the move is running on the node"
until [[ $(new get node "$NODE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null) == True ]]; do sleep 2; done
stamp "$NODE: Ready in $CLUSTER after $(( $(date +%s) - started )) s"
namespace=$(mgmt get clusters.rlinf.io "$CLUSTER" -o jsonpath='{.spec.agentNamespace}')
until [[ $(mgmt -n "$namespace" get nodeadmissions.station.rlark.io "$NODE" -o jsonpath='{.status.phase}') == Joined ]]; do sleep 2; done
stamp "$NODE: enrolled in $CLUSTER under its own identity after $(( $(date +%s) - started )) s"

# What it left behind.
[[ $mode == control-plane ]] || old delete node "$NODE" --ignore-not-found >/dev/null
mgmt -n "$old_namespace" delete networkenrollments.station.rlark.io,stationobservations.station.rlark.io "$NODE" --ignore-not-found >/dev/null
for station in $(mgmt -n "$old_namespace" get stations.station.rlark.io -o json | python3 -c '
import json, sys
print(" ".join(s["metadata"]["name"] for s in json.load(sys.stdin)["items"] if s["spec"]["binding"]["nodeName"] == sys.argv[1] and not s["spec"].get("retired")))' "$NODE"); do
  mgmt -n "$old_namespace" patch stations.station.rlark.io "$station" --type=merge -p '{"spec":{"retired":true}}' >/dev/null
done
stamp "$NODE: its Station in $old_cluster is retired, its enrollment there removed"
new get node "$NODE" -o wide
