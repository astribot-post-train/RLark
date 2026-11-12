#!/usr/bin/env bash
# Admit a member cluster whose API the management plane reaches only through
# the tunnel: a robot that carries its own single-node cluster behind a NAT.
# Run on the management machine as root, in this order:
#
#   admit-member.sh install    MEMBER_KUBECONFIG CLUSTER_ID
#   admit-member.sh bootstrap  MEMBER_KUBECONFIG CLUSTER_ID NODE LISTEN_PORT [COMPUTED.json]
#   admit-member.sh credential MEMBER_KUBECONFIG CLUSTER_ID MEMBER_SERVER [TLS_SERVER_NAME]
#   admit-member.sh rebind     MEMBER_KUBECONFIG CLUSTER_ID NODE REASON      (a replaced machine that keeps its name)
#   admit-member.sh finish     MEMBER_KUBECONFIG CLUSTER_ID NODE
#
# install    puts the reader identity, the API's CA, the agent and the gateway into the cluster.
# bootstrap  gives the node its first tunnel: its new public key is declared in the SiteNetwork
#            (the one copied key of the procedure) and the node gets a static document. For a
#            replaced machine COMPUTED.json is the document computed for the old one, saved
#            before its enrollment is deleted here; without it the document holds the hubs only.
# credential stores how station-api and station-controller reach the cluster's API (through the tunnel).
# rebind     moves the Station of a same-named node to the node's new UID; address and identity stay.
# finish     removes the declared key and the static document once the node has enrolled.
#
# A worker of a site cluster needs none of this: it joins the cluster and is labelled.
set -euo pipefail
: "${KCP:=/home/cailiang/.local/share/rlark-runtime/kcp-admin.kubeconfig}"
: "${REGISTRY:=10.254.240.1:5443}" "${API_URL:=https://10.11.14.128:18443}" "${API_CA:=/tmp/station-api-ca.crt}"
: "${AGENT_IMAGE:=$REGISTRY/rlark/station-agent@sha256:6c31551dcf42d7a49b108367e6ff948d67db11674e4ce6c591857bd4bedc3aba}"
: "${WIREGUARD_IMAGE:=$REGISTRY/rlark/station-wireguard@sha256:4fedb44376bdddf7df909aba8201661a5c0511df1c9c6444ef6ef08302b4bc4b}"
HERE=$(cd "$(dirname "$0")" && pwd)
command=$1 MEMBER=$2 CLUSTER=$3
mgmt() { kubectl --kubeconfig "$KCP" "$@"; }
member() { kubectl --kubeconfig "$MEMBER" "$@"; }
stamp() { echo "$(date +%T) $*"; }

case $command in
install)
  member apply -f "$HERE/../deploy/member-rbac.yaml"
  member -n station-system create secret generic station-api-ca --from-file=ca.crt="$API_CA" --dry-run=client -o yaml | member apply -f -
  sed -e "s#https://REPLACE_STATION_API_ENDPOINT#$API_URL#" -e "s#REPLACE_REGISTERED_CLUSTER_ID#$CLUSTER#" \
      -e "s#REPLACE_RLARK_IMAGE#$AGENT_IMAGE#" -e "s#REPLACE_PRIVATE_WIREGUARD_IMAGE#$WIREGUARD_IMAGE#" \
      "$HERE/../deploy/station-agent.yaml" | member apply -f -
  member label node --all --overwrite station.rlark.io/network=enabled
  member -n station-system rollout status ds/station-agent-network --timeout=300s
  stamp installed
  ;;
bootstrap)
  NODE=$4 PORT=$5 COMPUTED=${6:-}
  pod=$(member -n station-system get pod -l app=station-agent-network --field-selector spec.nodeName="$NODE" -o name)
  key=$(member -n station-system logs "$pod" -c wireguard | sed -n 's/.*wireguard public key: //p' | tail -1)
  [[ ${#key} == 44 ]]
  stamp "public key of $NODE: $key"
  mgmt -n station-system get sitenetwork default -o json > /tmp/sitenetwork.json
  mgmt get networkenrollments.station.rlark.io -A -o json > /tmp/enrollments.json
  python3 - "$CLUSTER" "$NODE" "$PORT" "$key" "$COMPUTED" > /tmp/bootstrap.json <<'PY'
import json, sys
cluster, node, port, key, computed = sys.argv[1:6]
spec = json.load(open('/tmp/sitenetwork.json'))['spec']
index = next(i for i, n in enumerate(spec['nodes']) if (n['clusterID'], n['nodeName']) == (cluster, node))
if computed:
    document = json.load(open(computed))
else:
    keys = {(e['spec']['clusterID'], e['metadata']['name']): e['status']['publicKey']
            for e in json.load(open('/tmp/enrollments.json'))['items'] if e.get('status', {}).get('phase') == 'Enrolled'}
    hubs = [dict(n, publicKey=keys[n['clusterID'], n['nodeName']]) for n in spec['nodes']
            if n.get('role') == 'Hub' and n.get('endpoints') and (n['clusterID'], n['nodeName']) in keys]
    document = {'interface': spec['interface'], 'address': spec['nodes'][index]['address'] + '/32', 'mtu': spec['mtu'],
                'peers': [{'publicKey': h['publicKey'], 'endpoint': h['endpoints'][0]['address'],
                           'allowedIPs': [h['address'] + '/32'], 'keepaliveSeconds': spec['keepaliveSeconds']} for h in hubs],
                'routes': [h['address'] + '/32' for h in hubs]}
document['listenPort'] = int(port)
json.dump(document, sys.stdout)
patch = [{'op': 'add', 'path': f'/spec/nodes/{index}/publicKey', 'value': key},
         {'op': 'add', 'path': f'/spec/nodes/{index}/listenPort', 'value': int(port)}]
json.dump(patch, open('/tmp/sitenetwork.patch.json', 'w'))
PY
  member -n station-system create configmap station-wireguard-config --from-file="$NODE.json"=/tmp/bootstrap.json --dry-run=client -o yaml | member apply -f -
  mgmt -n station-system patch sitenetwork default --type=json --patch-file /tmp/sitenetwork.patch.json
  # The replaced machine's enrollment would keep its key in every peer table.
  mgmt -n "$CLUSTER" delete networkenrollments.station.rlark.io "$NODE" --ignore-not-found
  member -n station-system delete "$pod"
  stamp "bootstrap document and declared key in place"
  ;;
credential)
  SERVER=$4 TLS_NAME=${5:-}
  member apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: station-api-reader-token
  namespace: station-system
  annotations: {kubernetes.io/service-account.name: station-api-reader}
type: kubernetes.io/service-account-token
EOF
  until token=$(member -n station-system get secret station-api-reader-token -o jsonpath='{.data.token}' | base64 -d) && [[ -n $token ]]; do sleep 1; done
  uid=$(member -n station-system get sa station-agent -o jsonpath='{.metadata.uid}')
  ca=$(member config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
  umask 077; work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
  cat > "$work/kubeconfig" <<EOF
apiVersion: v1
kind: Config
clusters: [{name: member, cluster: {server: "$SERVER", certificate-authority-data: $ca${TLS_NAME:+, tls-server-name: "$TLS_NAME"}}}]
users: [{name: reader, user: {token: $token}}]
contexts: [{name: member, context: {cluster: member, user: reader}}]
current-context: member
EOF
  kubectl --kubeconfig "$work/kubeconfig" -n station-system get sa station-agent -o name
  secret=$(python3 -c "import base64,hashlib,sys; print('station-member-'+base64.b32encode(hashlib.sha256(sys.argv[1].encode()).digest()[:10]).decode().rstrip('=').lower())" "$CLUSTER")
  mgmt -n station-member-credentials create secret generic "$secret" --from-file=kubeconfig="$work/kubeconfig" \
    --from-literal=service-account-namespace=station-system --from-literal=service-account-name=station-agent \
    --from-literal=service-account-uid="$uid" --from-literal=audience=station.rlark.io --dry-run=client -o yaml | mgmt apply -f -
  mgmt -n station-member-credentials annotate secret "$secret" --overwrite station.rlark.io/cluster-id="$CLUSTER"
  stamp "member credential $secret stored"
  ;;
rebind)
  NODE=$4 REASON=$5
  uid=$(member get node "$NODE" -o jsonpath='{.metadata.uid}')
  read -r station station_uid version < <(mgmt -n "$CLUSTER" get stations.station.rlark.io -o json | python3 -c "
import json, sys
s = next(s for s in json.load(sys.stdin)['items'] if s['spec']['binding']['nodeName'] == sys.argv[1] and not s['spec'].get('retired'))
print(s['metadata']['name'], s['metadata']['uid'], s['spec']['binding']['version'])" "$NODE")
  request=replace-$NODE-$(date +%Y%m%d%H%M%S)
  mgmt apply -f - <<EOF
apiVersion: station.rlark.io/v1alpha1
kind: StationRebind
metadata: {name: $request, namespace: $CLUSTER}
spec:
  requestID: $request
  stationRef: {name: $station, uid: $station_uid}
  expectedBindingVersion: $version
  targetNodeName: $NODE
  targetNodeUID: $uid
  reason: "$REASON"
  requestedBy: ${SUDO_USER:-root}@$(hostname)
EOF
  stamp "rebind $request submitted: $station -> node UID $uid"
  until phase=$(mgmt -n "$CLUSTER" get stationrebinds.station.rlark.io "$request" -o jsonpath='{.status.phase}') && [[ $phase == Applied || $phase == Rejected ]]; do sleep 1; done
  stamp "rebind $phase"
  [[ $phase == Applied ]]
  until [[ $(mgmt -n "$CLUSTER" get networkenrollments.station.rlark.io "$NODE" -o jsonpath='{.status.phase}' 2>/dev/null) == Enrolled ]]; do sleep 1; done
  stamp "$NODE enrolled"
  ;;
finish)
  NODE=$4
  [[ $(mgmt -n "$CLUSTER" get networkenrollments.station.rlark.io "$NODE" -o jsonpath='{.status.phase}') == Enrolled ]]
  index=$(mgmt -n station-system get sitenetwork default -o json | python3 -c "
import json, sys
print(next(i for i, n in enumerate(json.load(sys.stdin)['spec']['nodes']) if (n['clusterID'], n['nodeName']) == tuple(sys.argv[1:3])))" "$CLUSTER" "$NODE")
  mgmt -n station-system patch sitenetwork default --type=json -p "[{\"op\":\"remove\",\"path\":\"/spec/nodes/$index/publicKey\"}]"
  member -n station-system delete configmap station-wireguard-config
  stamp "declared key and static document removed"
  ;;
*) sed -n 2,22p "$0"; exit 2 ;;
esac
