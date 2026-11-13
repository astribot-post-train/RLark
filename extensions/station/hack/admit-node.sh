#!/usr/bin/env bash
# Admit a machine as a node of a registered cluster and print what to run on
# it: one command, and before it, for a machine of a site whose leaves stand
# behind a device of their own (the site's leafUplink.setup), the command that
# makes that device let the machine out. Run where the management plane's
# kubeconfig is.
#
#   admit-node.sh CLUSTER_ID NODE_NAME API_SERVER STATION_API [label=value ...]
#
#   API_SERVER   host:port of the cluster's API as the machine reaches it without the tunnel
#   STATION_API  host:port of Station API as the machine reaches it
# Environment: KCP (management kubeconfig), CA (Station API's CA file), TAINTS
# ("k=v:Effect,..."), ADDRESS (pin the tunnel address), UPLINK
# ("gateway,source,interface" for a machine whose default route does not lead
# out), TTL (default "2 hours").
set -euo pipefail
: "${KCP:=/home/cailiang/.local/share/rlark-runtime/kcp-admin.kubeconfig}" "${CA:=/tmp/station-api-ca.crt}"
CLUSTER=$1 NODE=$2 API_SERVER=$3 STATION_API=$4; shift 4
# What the operator runs as root is what this API gave. The machine has no CA
# of ours yet, so its curl is told the public key of the API's certificate
# instead, seen here over a connection the CA verifies (-k leaves the chain
# unchecked; the key is checked whatever -k says).
pin=$(openssl s_client -connect "$STATION_API" -CAfile "$CA" -verify_return_error </dev/null 2>/dev/null |
  openssl x509 -pubkey -noout 2>/dev/null | openssl pkey -pubin -outform der 2>/dev/null | openssl dgst -sha256 -binary | base64) || { echo "Station API at $STATION_API shows no certificate that the CA in $CA vouches for; nothing was admitted" >&2; exit 1; }
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
  [[ -z ${ADDRESS:-} ]] || echo "  address: $ADDRESS"
  [[ -z ${UPLINK:-} ]] || { IFS=, read -r gateway source interface <<<"$UPLINK"; echo "  uplink: {gateway: $gateway, source: $source, interface: $interface}"; }
  [[ -z ${TAINTS:-} ]] || echo "  taints: [\"${TAINTS//,/\", \"}\"]"
  [[ $# == 0 ]] || { echo "  labels:"; for label in "$@"; do echo "    ${label%%=*}: \"${label#*=}\""; done; }
  # A node that has an admission keeps it: for a node that joined by a code
  # it is what declares its tunnel address. The new code opens it again.
  echo "  publicKey: null"
  echo "  redeemedAt: null"
} > "${admission:=$(mktemp)}"
if kubectl --kubeconfig "$KCP" -n "$namespace" get nodeadmissions.station.rlark.io "$NODE" >/dev/null 2>&1; then
  kubectl --kubeconfig "$KCP" -n "$namespace" patch nodeadmissions.station.rlark.io "$NODE" --type=merge --patch-file "$admission" >&2
else
  grep -v ': null$' "$admission" | kubectl --kubeconfig "$KCP" create -f - >&2
fi
rm -f "$admission"

fetch="curl -fsSk --pinnedpubkey sha256//$pin https://$STATION_API/b/$code"
# The device before the machine fetches its setup with the machine's code and
# runs it only if it is, byte for byte, the file this API serves now.
read -r gateway user < <(kubectl --kubeconfig "$KCP" -n station-system get sitenetwork default -o json | python3 -c '
import json, sys
for site in json.load(sys.stdin)["spec"]["sites"]:
    uplink = site.get("leafUplink", {})
    if sys.argv[1] in site.get("clusters", []) and "setup" in uplink:
        print(uplink["gateway"], uplink["setup"]["user"])' "$CLUSTER") || true
if [[ -n ${gateway:-} ]]; then
  digest=$(curl -fsS --cacert "$CA" "https://$STATION_API/b/$code/uplink" | sha256sum | cut -d' ' -f1)
  echo "ssh -t $user@$gateway 'f=\$(mktemp) && $fetch/uplink -o \$f && echo \"$digest  \$f\" | sha256sum -c - && sudo sh \$f'"
fi
echo "$fetch | sudo sh"
