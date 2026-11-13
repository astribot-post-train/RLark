#!/usr/bin/env bash
# Admit a machine as a node of a registered cluster and print the one command
# to run on it. Run where the management plane's kubeconfig is.
#
#   admit-node.sh CLUSTER_ID NODE_NAME API_SERVER STATION_API [label=value ...]
#
#   API_SERVER   host:port of the cluster's API as the machine reaches it without the tunnel;
#                where the host is the control plane's name, API_ADDRESS is the address
#                the machine resolves it to (one marked line in its /etc/hosts)
#   STATION_API  host:port of Station API as the machine reaches it
# Environment: KCP (management kubeconfig), API_ADDRESS, TAINTS ("k=v:Effect,..."), ADDRESS
# (pin the tunnel address), LISTEN_PORT, UPLINK ("gateway,source,interface" for
# a machine whose default route does not lead out), TTL (default "2 hours").
# The Gateway's "add node" page does the same and shows the same command.
set -euo pipefail
: "${KCP:=/home/cailiang/.local/share/rlark-runtime/kcp-admin.kubeconfig}"
CLUSTER=$1 NODE=$2 API_SERVER=$3 STATION_API=$4; shift 4
namespace=$(kubectl --kubeconfig "$KCP" get clusters.rlinf.io "$CLUSTER" -o jsonpath='{.spec.agentNamespace}')
code=$(head -c 512 /dev/urandom | tr -dc 'a-z0-9' | cut -c1-8)
{
  cat <<ADMISSION
apiVersion: station.rlark.io/v1alpha1
kind: NodeAdmission
metadata: {name: $NODE, namespace: $namespace}
spec:
  clusterID: $CLUSTER
  codeHash: $(printf %s "$code" | sha256sum | cut -d' ' -f1)
  expiresAt: "$(date -u -d "+${TTL:-2 hours}" +%Y-%m-%dT%H:%M:%SZ)"
  apiServer: $API_SERVER
ADMISSION
  [[ -z ${API_ADDRESS:-} ]] || echo "  apiServerAddress: $API_ADDRESS"
  [[ -z ${ADDRESS:-} ]] || echo "  address: $ADDRESS"
  [[ -z ${LISTEN_PORT:-} ]] || echo "  listenPort: $LISTEN_PORT"
  [[ -z ${UPLINK:-} ]] || { IFS=, read -r gateway source interface <<<"$UPLINK"; echo "  uplink: {gateway: $gateway, source: $source, interface: $interface}"; }
  [[ -z ${TAINTS:-} ]] || echo "  taints: [\"${TAINTS//,/\", \"}\"]"
  [[ $# == 0 ]] || { echo "  labels:"; for label in "$@"; do echo "    ${label%%=*}: \"${label#*=}\""; done; }
} | kubectl --kubeconfig "$KCP" apply -f - >&2
echo "curl -sk https://$STATION_API/b/$code | sudo sh"
