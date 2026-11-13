#!/usr/bin/env bash
# Register a member cluster with the site's Station: what the cluster runs for
# it, and how the management plane reaches the cluster's API. Run on the
# management machine as root, in this order:
#
#   admit-member.sh install    MEMBER_KUBECONFIG CLUSTER_ID
#   admit-member.sh credential MEMBER_KUBECONFIG CLUSTER_ID MEMBER_SERVER [TLS_SERVER_NAME]
#
# install    puts the reader identity, the API's CA, the agent and the gateway into the cluster.
# credential stores how station-api and station-controller reach the cluster's API (through the
#            tunnel: MEMBER_SERVER is the control plane's tunnel address, TLS_SERVER_NAME its name).
#            Run again when the cluster's control plane has moved to another machine.
#
# A machine joins a cluster as a node by admit-node.sh; a robot that carries a
# cluster of its own is not a form the site has any more.
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
*) sed -n 2,14p "$0"; exit 2 ;;
esac
