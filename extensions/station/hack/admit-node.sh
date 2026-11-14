#!/usr/bin/env bash
# Admit a machine as a node of a registered cluster and print the one line to
# run on it. For a machine of a site whose leaves stand behind a device of
# their own (the site's leafUplink.setup), the line first makes that device
# let the machine out (it asks for the device's password), then installs.
# Run where the management plane's kubeconfig is.
#
#   admit-node.sh CLUSTER_ID NODE_NAME API_SERVER STATION_API [label=value ...]
#
#   API_SERVER   host:port of the cluster's API as the machine reaches it without the tunnel
#   STATION_API  host:port of Station API as the machine reaches it
# Environment: KCP (management kubeconfig), CA (Station API's CA file), TAINTS
# ("k=v:Effect,..."), SITE (the site the machine stands in, when it is not its
# cluster's), ADDRESS (pin the tunnel address), UPLINK
# (the gateway of a machine whose default route does not lead out, where its
# site declares no leafUplink), TTL (default "2 hours").
set -euo pipefail
: "${KCP:=/home/cailiang/.local/share/rlark-runtime/kcp-admin.kubeconfig}" "${CA:=/tmp/station-api-ca.crt}"
CLUSTER=$1 NODE=$2 API_SERVER=$3 STATION_API=$4; shift 4
# What the operator runs as root is what this API gave. The machine has no CA
# of ours yet: its command fetches the CA unverified, compares it with the
# digest printed here, and verifies every connection after that with it. A
# renewed certificate of the API leaves a command good; another CA does not.
ca_digest=$(sha256sum < "$CA" | cut -d' ' -f1)
[[ $(curl -fsS --cacert "$CA" "https://$STATION_API/v1/bootstrap/ca" | sha256sum | cut -d' ' -f1) == "$ca_digest" ]] ||
  { echo "Station API at $STATION_API does not serve, over a connection the CA in $CA verifies, that same CA; nothing was admitted" >&2; exit 1; }
[[ -z ${SITE:-} ]] || kubectl --kubeconfig "$KCP" -n station-system get sitenetwork default -o jsonpath='{range .spec.sites[*]}{.name}{"\n"}{end}' | grep -qx "$SITE" ||
  { echo "the SiteNetwork has no site $SITE; nothing was admitted" >&2; exit 1; }
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
  echo "  site: ${SITE:-null}"
  [[ -z ${ADDRESS:-} ]] || echo "  address: $ADDRESS"
  [[ -z ${UPLINK:-} ]] || { echo "  uplink: {gateway: $UPLINK}"; }
  [[ -z ${TAINTS:-} ]] || echo "  taints: [\"${TAINTS//,/\", \"}\"]"
  [[ $# == 0 ]] || { echo "  labels:"; for label in "$@"; do echo "    ${label%%=*}: \"${label#*=}\""; done; }
  # A node that has an admission keeps it: for a node that joined by a code
  # it is what declares its tunnel address. The new code opens it again.
  echo "  publicKey: null"
  echo "  redeemedAt: null"
} > "${admission:=$(mktemp)}"
if kubectl --kubeconfig "$KCP" -n "$namespace" get nodeadmissions.station.rlark.io "$NODE" >/dev/null 2>&1; then
  kubectl --kubeconfig "$KCP" -n "$namespace" patch nodeadmissions.station.rlark.io "$NODE" --type=merge --patch-file "$admission" >&2
  # The labels are the ones given now, as one value: a label of the earlier admission that is left out is gone.
  labels=$(python3 -c 'import json, sys; print(json.dumps(dict(a.split("=", 1) for a in sys.argv[1:])))' "$@")
  kubectl --kubeconfig "$KCP" -n "$namespace" patch nodeadmissions.station.rlark.io "$NODE" --type=json -p "[{\"op\":\"add\",\"path\":\"/spec/labels\",\"value\":$labels}]" >&2
else
  grep -v ': null$' "$admission" | kubectl --kubeconfig "$KCP" create -f - >&2
fi
rm -f "$admission"

trust="c=\$(mktemp) && curl --connect-timeout 15 -fsSk https://$STATION_API/v1/bootstrap/ca -o \$c && echo \"$ca_digest  \$c\" | sha256sum -c - && curl -fsS --cacert \$c https://$STATION_API/b/$code"
# The device before the machine fetches its setup with the machine's code and
# runs it only if it is, byte for byte, the file this API serves now.
read -r gateway login < <(kubectl --kubeconfig "$KCP" -n station-system get sitenetwork default -o json | python3 -c '
import json, sys
spec, (cluster, node, named) = json.load(sys.stdin)["spec"], sys.argv[1:]
for entry in spec.get("nodes", []):
    if (entry["clusterID"], entry["nodeName"]) == (cluster, node):
        named = entry.get("site", named)
for site in spec["sites"]:
    uplink = site.get("leafUplink", {})
    if (site["name"] == named if named else cluster in site.get("clusters", [])) and "setup" in uplink:
        print(uplink["gateway"], uplink["setup"]["login"])' "$CLUSTER" "$NODE" "${SITE:-}") || true
# One line for the operator either way: on such a site it lets the machine out through its device first, then installs.
first=
if [[ -n ${gateway:-} ]]; then
  digest=$(curl -fsS --cacert "$CA" "https://$STATION_API/b/$code/uplink" | sha256sum | cut -d' ' -f1)
  first="ssh -t $login@$gateway 'f=\$(mktemp) $trust/uplink -o \$f && echo \"$digest  \$f\" | sha256sum -c - && sudo sh \$f' && "
fi
echo "$first$trust | sudo sh"
