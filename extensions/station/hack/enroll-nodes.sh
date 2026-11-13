#!/usr/bin/env bash
# Admit a number of machines as nodes of one class with a single code, and
# print the one command to run on each of them. Run where the management
# plane's kubeconfig is. Each machine joins under its own host name.
#
#   enroll-nodes.sh CLUSTER_ID NODE_CLASS MAX_USES API_SERVER STATION_API
#
#   NODE_CLASS   name of a NodeClass: the labels and taints the nodes register with
#   API_SERVER   host:port of the cluster's API as the machines reach it without the tunnel
#   STATION_API  host:port of Station API as the machines reach it
# Environment: KCP (management kubeconfig), TTL (default "2 hours"), NAME
# (default enroll-<time>). One machine with settings of its own (a pinned
# tunnel address, an uplink) is admit-node.sh.
set -euo pipefail
: "${KCP:=/home/cailiang/.local/share/rlark-runtime/kcp-admin.kubeconfig}"
CLUSTER=$1 CLASS=$2 MAX_USES=$3 API_SERVER=$4 STATION_API=$5
namespace=$(kubectl --kubeconfig "$KCP" get clusters.rlinf.io "$CLUSTER" -o jsonpath='{.spec.agentNamespace}')
kubectl --kubeconfig "$KCP" get nodeclasses.station.rlark.io "$CLASS" >/dev/null
code=$(head -c 512 /dev/urandom | tr -dc 'a-z0-9' | cut -c1-12)
kubectl --kubeconfig "$KCP" apply -f - >&2 <<ENROLLMENT
apiVersion: station.rlark.io/v1alpha1
kind: NodeEnrollment
metadata: {name: ${NAME:-enroll-$(date -u +%Y%m%d-%H%M%S)}, namespace: $namespace}
spec:
  clusterID: $CLUSTER
  codeHash: $(printf %s "$code" | sha256sum | cut -d' ' -f1)
  expiresAt: "$(date -u -d "+${TTL:-2 hours}" +%Y-%m-%dT%H:%M:%SZ)"
  maxUses: $MAX_USES
  nodeClass: $CLASS
  apiServer: $API_SERVER
ENROLLMENT
echo "curl -sk https://$STATION_API/b/$code | sudo sh"
