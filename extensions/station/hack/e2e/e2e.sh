#!/bin/bash
# A throwaway copy of the whole Station network on one machine: kcp as the
# management plane, PostgreSQL, station-api and station-controller as
# containers, and three kind clusters whose nodes are the sites' machines.
# Every node runs the real station-agent-network pod, so WireGuard interfaces
# come up in the node containers' own network namespaces.
#
#   e2e.sh up      create everything (about 6 minutes)
#   e2e.sh plane   restart only the management plane, keeping the clusters
#   e2e.sh down    remove everything
#
# Needs docker, kind, kubectl, openssl, python3 and the host's wireguard
# module. Nothing here touches a real site: all names start with e2e-.
# Images: IMAGES_TAG names station-v2/{station-services,station-controller,
# station-agent,station-wireguard} in the local docker.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../../.." && pwd)
D=${E2E_DIR:-$HOME/.cache/station-e2e}
TAG=${IMAGES_TAG:-e2e-1}
NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.31.0}
AGENT_IMAGE=${AGENT_IMAGE:-station-v2/station-agent:$TAG}
WIREGUARD_IMAGE=${WIREGUARD_IMAGE:-station-v2/station-wireguard:$TAG}
# cluster ID=number of nodes (the first is the control plane, which also runs pods).
# Every node container needs inotify instances: with fs.inotify.max_user_instances=128
# only about three start; raise it to 8192 for five.
declare -A CLUSTERS
for entry in ${E2E_CLUSTERS:-e2e-office=2 e2e-lab=1 e2e-ksyun=2}; do CLUSTERS[${entry%=*}]=${entry#*=}; done

mgmt() { kubectl --kubeconfig "$D/kcp.kubeconfig" "$@"; }
member() { local cluster=$1; shift; kubectl --kubeconfig "$D/$cluster.kubeconfig" "$@"; }
ip_of() { docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$1"; }

down() {
  for cluster in "${!CLUSTERS[@]}" ${EXTRA_CLUSTERS:-}; do kind delete cluster --name "$cluster" >/dev/null 2>&1 || true; done
  docker rm -f e2e-station-api e2e-station-controller e2e-kcp e2e-station-postgres >/dev/null 2>&1 || true
  rm -rf "$D"
}

certificates() {
  mkdir -p "$D/pki" && cd "$D/pki"
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=station-e2e-ca" -keyout ca.key -out ca.crt 2>/dev/null
  for name in e2e-station-api e2e-station-postgres; do
    openssl req -newkey rsa:2048 -nodes -subj "/CN=$name" -keyout $name.key -out $name.csr 2>/dev/null
    openssl x509 -req -in $name.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 30 -out $name.crt \
      -extfile <(echo "subjectAltName=DNS:$name") 2>/dev/null
  done
  chmod 644 ./*.key
}

create_cluster() { # cluster nodes
  local cluster=$1 nodes=$2 config="$D/$1.kind.yaml"
  # /boot: kubeadm's system verification reads the kernel configuration, which a node container lacks.
  local boot='  extraMounts: [{hostPath: /boot, containerPath: /boot, readOnly: true}]'
  { echo "kind: Cluster"; echo "apiVersion: kind.x-k8s.io/v1alpha4"; echo "nodes:"; echo "- role: control-plane"; echo "$boot"
    for _ in $(seq 2 "$nodes"); do echo "- role: worker"; echo "$boot"; done; echo "${KIND_NETWORKING:-}"; } > "$config"
  kind create cluster --name "$cluster" --image "$NODE_IMAGE" --config "$config" --kubeconfig "$D/$cluster.kubeconfig" --wait "${KIND_WAIT:-120s}" 2>&1 | tail -2
  load "$cluster"
}

# register makes the cluster a member: the reader identity station-api uses in
# it, the Cluster object and the credential Secret on the management plane.
register() { # cluster
  local cluster=$1
  member "$cluster" apply -f "$REPO/extensions/station/deploy/member-rbac.yaml" >/dev/null
  local token uid ca
  token=$(member "$cluster" -n station-system create token station-api-reader --duration 48h)
  uid=$(member "$cluster" -n station-system get sa station-agent -o jsonpath='{.metadata.uid}')
  ca=$(member "$cluster" config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
  cat > "$D/$cluster.member.kubeconfig" <<EOF
apiVersion: v1
kind: Config
clusters: [{name: member, cluster: {server: "https://$cluster-control-plane:6443", certificate-authority-data: $ca}}]
users: [{name: reader, user: {token: $token}}]
contexts: [{name: member, context: {cluster: member, user: reader}}]
current-context: member
EOF
  local secret
  secret=$(python3 -c "import base64,hashlib,sys; print('station-member-'+base64.b32encode(hashlib.sha256(sys.argv[1].encode()).digest()[:10]).decode().rstrip('=').lower())" "$cluster")
  mgmt create namespace "$cluster" --dry-run=client -o yaml | mgmt apply -f - >/dev/null
  mgmt apply -f - >/dev/null <<EOF
apiVersion: rlinf.io/v1alpha1
kind: Cluster
metadata: {name: $cluster}
spec: {agentNamespace: $cluster}
EOF
  mgmt -n station-member-credentials create secret generic "$secret" --from-file=kubeconfig="$D/$cluster.member.kubeconfig" \
    --from-literal=service-account-namespace=station-system --from-literal=service-account-name=station-agent \
    --from-literal=service-account-uid="$uid" --from-literal=audience=station.rlark.io --dry-run=client -o yaml | mgmt apply -f - >/dev/null
  mgmt -n station-member-credentials annotate secret "$secret" --overwrite station.rlark.io/cluster-id="$cluster" >/dev/null
  member "$cluster" -n station-system create secret generic station-api-ca --from-file=ca.crt="$D/pki/ca.crt" --dry-run=client -o yaml | member "$cluster" apply -f - >/dev/null
}

# agents installs the repository's DaemonSets with the images and endpoint filled in.
agents() { # cluster
  sed -e "s#REPLACE_STATION_API_ENDPOINT#e2e-station-api:18443#" -e "s#REPLACE_REGISTERED_CLUSTER_ID#$1#" \
      -e "s#REPLACE_RLARK_IMAGE#$AGENT_IMAGE#" -e "s#REPLACE_PRIVATE_WIREGUARD_IMAGE#$WIREGUARD_IMAGE#" \
      "${AGENT_MANIFEST:-$REPO/extensions/station/deploy/station-agent.yaml}" | member "$1" apply -f - >/dev/null
}

# services (re)starts station-controller and station-api from the images of IMAGES_TAG.
services() {
  docker rm -f e2e-station-api e2e-station-controller >/dev/null 2>&1 || true
  docker run -d --name e2e-station-controller --network kind -v "$D/kcp.kubeconfig:/kubeconfig:ro" station-v2/station-controller:"$TAG" \
    --management-kubeconfig=/kubeconfig --member-credentials-namespace=station-member-credentials >/dev/null
  docker run -d --name e2e-station-api --network kind -v "$D/kcp.kubeconfig:/kubeconfig:ro" -v "$D/pki:/pki:ro" \
    -e STATION_API_DATABASE_DSN="postgres://postgres:e2e@e2e-station-postgres:5432/station?sslmode=verify-full&sslrootcert=/pki/ca.crt" \
    station-v2/station-services:"$TAG" /usr/local/bin/station-api --addr=:18443 --kubeconfig=/kubeconfig \
    --member-credentials-namespace=station-member-credentials --tls-cert-file=/pki/e2e-station-api.crt --tls-key-file=/pki/e2e-station-api.key ${STATION_API_ARGS:-} >/dev/null
}

# load puts the agent and gateway images of IMAGES_TAG on every node of a cluster.
load() { kind load docker-image --name "$1" "$AGENT_IMAGE" "$WIREGUARD_IMAGE" >/dev/null; }

# plane starts the management plane and makes every existing cluster a member.
plane() {
  docker rm -f e2e-station-api e2e-station-controller e2e-kcp e2e-station-postgres >/dev/null 2>&1 || true
  docker run -d --name e2e-station-postgres --network kind -e POSTGRES_PASSWORD=e2e -e POSTGRES_DB=station \
    -v "$D/pki:/pki:ro" --entrypoint bash postgres:15 -c \
    'cp /pki/e2e-station-postgres.crt /pki/e2e-station-postgres.key /var/lib/postgresql/ && chown postgres /var/lib/postgresql/e2e-* && chmod 600 /var/lib/postgresql/*.key &&
     exec docker-entrypoint.sh postgres -c ssl=on -c ssl_cert_file=/var/lib/postgresql/e2e-station-postgres.crt -c ssl_key_file=/var/lib/postgresql/e2e-station-postgres.key' >/dev/null
  docker run -d --name e2e-kcp --network kind --user 0 ghcr.io/kcp-dev/kcp:v0.30.0 start --root-directory=/data >/dev/null
  until docker cp e2e-kcp:/data/admin.kubeconfig "$D/kcp-admin.kubeconfig" 2>/dev/null && [[ -s $D/kcp-admin.kubeconfig ]] &&
    docker exec e2e-station-postgres pg_isready -q -U postgres; do sleep 1; done
  # kcp's serving certificate names its container address; the host and the other containers both reach it there.
  sed -E "s#server: https://[^/]+:6443#server: https://$(ip_of e2e-kcp):6443#" "$D/kcp-admin.kubeconfig" > "$D/kcp.kubeconfig"
  kubectl --kubeconfig "$D/kcp.kubeconfig" config use-context root >/dev/null
  until mgmt get namespaces >/dev/null 2>&1; do sleep 1; done

  mgmt apply -f "$REPO/api/config/crd/bases/rlinf.io_clusters.yaml" -f "$REPO/api/config/crd/bases/rlinf.io_nodes.yaml" -f "$REPO/extensions/station/config/crd/bases" >/dev/null
  mgmt wait --for condition=Established crd --all --timeout 60s >/dev/null
  for namespace in station-system station-member-credentials; do mgmt create namespace $namespace >/dev/null; done
  docker exec -i e2e-station-postgres psql -q -U postgres -d station < "$REPO/apps/rlark/config/station-api-challenges.sql"

  for cluster in "${!CLUSTERS[@]}"; do register "$cluster"; done

  services
  echo "plane: management plane at $D/kcp.kubeconfig, members at $D/<cluster>.kubeconfig"
}

# site_network declares the three sites the way the live site of 2026-11-12 is
# declared: an office with two hubs that mesh (the second on a reported
# address), a lab with a leaf, a cloud site with two hubs that reach each other
# on their own "VPC" addresses and are dialled on another one.
# Addresses are from 198.18.0.0/15 (reserved for benchmarking): nothing on a
# real network answers them, so a ping that leaves the tunnel is a lost ping.
site_network() {
  local dev k57 k196
  dev=$(ip_of e2e-office-control-plane) k57=$(ip_of e2e-ksyun-control-plane) k196=$(ip_of e2e-ksyun-worker)
  docker exec e2e-ksyun-control-plane sh -c "ip addr replace 198.19.2.57/32 dev lo && ip route replace 198.19.2.196/32 via $k196"
  docker exec e2e-ksyun-worker sh -c "ip addr replace 198.19.2.196/32 dev lo && ip route replace 198.19.2.57/32 via $k57"
  docker exec e2e-office-control-plane ip addr replace 198.19.5.12/32 dev lo # stands for the NAS behind the office hub
  mgmt apply -f - <<EOF
apiVersion: station.rlark.io/v1alpha1
kind: SiteNetwork
metadata: {name: default, namespace: station-system}
spec:
  mode: ${1:-Tunnel}
  pool: 198.18.99.0/24
  sites:
  - {name: office, clusters: [e2e-office], mesh: true}
  - {name: lab, clusters: [e2e-lab], reaches: [office]}
  - {name: ksyun, clusters: [e2e-ksyun]}
  nodes:
  - clusterID: e2e-office
    nodeName: e2e-office-control-plane
    role: Hub
    address: 198.18.99.1
    endpoints: [{address: "$dev:51820", scope: Site}]
    advertise: [{cidr: 198.19.5.12/32, sites: [ksyun]}]
  - clusterID: e2e-ksyun
    nodeName: e2e-ksyun-control-plane
    role: Hub
    address: 198.18.99.2
    nodeAddress: 198.19.2.57
    endpoints: [{address: "$k57:51820", scope: Public}]
    advertise: [{cidr: 172.27.0.0/24, sites: [office]}]
  - clusterID: e2e-lab
    nodeName: e2e-lab-control-plane
    role: Leaf
    address: 198.18.99.3
  - clusterID: e2e-ksyun
    nodeName: e2e-ksyun-worker
    role: Hub
    address: 198.18.99.4
    nodeAddress: 198.19.2.196
    interface: st-hub0
    listenPort: 51821
    endpoints: [{address: "$k196:51821", scope: Public}]
    advertise: [{cidr: 172.27.1.0/24, sites: [office]}]
  - clusterID: e2e-office
    nodeName: e2e-office-worker
    role: Hub
    address: 198.18.99.5
    endpoints: [{scope: Site}]
EOF
}

# enable labels every node of a cluster into the network.
enable() { member "$1" label nodes --all --overwrite station.rlark.io/network=enabled >/dev/null; }

# ping runs busybox ping in a node's network namespace: ping NODE ARGS...
ping() { local node=$1; shift; docker run --rm --network "container:$node" busybox:1.36 ping "$@"; }

# wg shows a node's tunnel as the gateway sees it: wg CLUSTER NODE
wg() { member "$1" -n station-system exec "$(member "$1" -n station-system get pods -o name --field-selector spec.nodeName="$2" -l app=station-agent-network)" -c wireguard -- /station-gateway --status; }

up() {
  down
  certificates
  for cluster in "${!CLUSTERS[@]}"; do create_cluster "$cluster" "${CLUSTERS[$cluster]}"; done
  plane
  for cluster in "${!CLUSTERS[@]}"; do agents "$cluster"; done
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  case ${1:-} in
  up) up ;;
  down) down ;;
  *) "$@" ;;
  esac
fi
