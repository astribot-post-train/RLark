#!/bin/bash
# Isolated run of the site's cut-over: the site cluster (e2e-office: the site
# entry and a second hub, v1.31, Calico v3.28 over the nodes' own addresses)
# goes to v1.32, to Calico v3.30 over the tunnel, and to v1.33; then a robot
# cluster (e2e-lab: a control plane and a worker at v1.31, leaves of the site
# network) moves node by node into it with hack/migrate-node.sh, the worker
# first. The robots stand behind a NAT: nothing but what they dial themselves
# passes between them and the site's nodes outside the tunnel.
#
#   move.sh up              both clusters with Calico, tunnels up, NAT rules on the robots
#   move.sh upgrade         the site cluster to VERSION, Calico changed in between
#   move.sh installer       the installer of VERSION with what a robot needs before it can pull
#   move.sh move NODE       move one node (the control plane with CONTROL_PLANE=yes)
#   move.sh check           nodes, tunnels, pod network and cluster DNS from the robots
#   move.sh down
#
# Needs the upgrade bundles of hack/e2e/upgrade.sh (bundle v1.32.13, bundle
# v1.33.13), the Station images of IMAGES_TAG (hack/e2e/build-local.sh), the
# Calico manifests in CALICO_DIR/<version>/calico.yaml and their images in docker.
set -euo pipefail
export E2E_CLUSTERS="e2e-office=2 e2e-lab=2" IMAGES_TAG=${IMAGES_TAG:-e2e-move}
HERE=$(cd "$(dirname "$0")" && pwd)
source "$HERE/e2e.sh"
VERSION=${VERSION:-v1.33.13}
KUBE_DIR=${KUBE_DIR:-$REPO/../kube}
CALICO_DIR=${CALICO_DIR:-$REPO/../calico}
CALICO_OLD=v3.28.2 CALICO_NEW=v3.30.7
INSTALLER=rlark/node-bootstrap:$TAG
HUB=e2e-office-control-plane
declare -A PODS=([e2e-office]=10.244.0.0/16 [e2e-lab]=10.242.0.0/16)
# import puts images on every node of a cluster. kind load asks for every
# platform of a multi-platform image, and docker holds only this machine's.
import() { # cluster image...
  local cluster=$1 node image; shift
  for node in $(kind get nodes --name "$cluster"); do
    for image in "$@"; do docker save --platform linux/amd64 "$image" | docker exec -i "$node" ctr -n k8s.io images import - >/dev/null; done
  done
}
calico_images() { echo docker.io/calico/node:$1 docker.io/calico/cni:$1 docker.io/calico/kube-controllers:$1; }

# calico prints the manifest of a version the way the site installs it:
# METHOD is how a node finds the address it peers and encapsulates with.
calico() { # version cluster method mtu
  sed -e "s#veth_mtu: \"0\"#veth_mtu: \"$4\"#" \
      -e "s#^\( *\)- name: CALICO_IPV4POOL_IPIP#\1- name: IP_AUTODETECTION_METHOD\n\1  value: \"$3\"\n\1- name: CALICO_IPV4POOL_CIDR\n\1  value: \"${PODS[$2]}\"\n\1- name: FELIX_IGNORELOOSERPF\n\1  value: \"true\"\n&#" \
      "$CALICO_DIR/$1/calico.yaml"
}
calico_ready() { # cluster
  member "$1" -n kube-system rollout status ds/calico-node --timeout 300s >/dev/null
  member "$1" wait --for=condition=Ready node --all --timeout=180s >/dev/null
}

up() {
  export STATION_API_ARGS="--bootstrap-ca-file=/pki/ca.crt --bootstrap-image-file=/pki/installer.tar --bootstrap-image-ref=$INSTALLER"
  down; certificates; mkdir -p "$D/pki"; : > "$D/pki/installer.tar"
  for cluster in e2e-office e2e-lab; do
    KIND_NETWORKING="networking: {disableDefaultCNI: true, podSubnet: ${PODS[$cluster]}}" KIND_WAIT=0s create_cluster "$cluster" 2
    import "$cluster" $(calico_images $CALICO_OLD) busybox:1.36
    calico $CALICO_OLD "$cluster" kubernetes-internal-ip 0 | member "$cluster" apply -f - >/dev/null
    calico_ready "$cluster"
  done
  # kind gives its own nodes a /kubelet cgroup; a machine that joins has none, as on a real site.
  member e2e-office -n kube-system get cm kubelet-config -o yaml | sed '/^    cgroupRoot: /d' | member e2e-office apply -f - >/dev/null
  plane
  for cluster in e2e-office e2e-lab; do agents "$cluster"; enable "$cluster"; done
  mgmt apply -f - <<SITE
apiVersion: station.rlark.io/v1alpha1
kind: SiteNetwork
metadata: {name: default, namespace: station-system}
spec:
  mode: Tunnel
  pool: 198.18.99.0/24
  mtu: 1380
  sites:
  - {name: office, clusters: [e2e-office], mesh: true}
  - {name: lab, clusters: [e2e-lab], reaches: [office]}
  nodes:
  - clusterID: e2e-office
    nodeName: $HUB
    role: Hub
    relay: true
    address: 198.18.99.1
    endpoints: [{address: "$(ip_of $HUB):51820", scope: Site}]
  - {clusterID: e2e-office, nodeName: e2e-office-worker, role: Hub, address: 198.18.99.5, endpoints: [{scope: Site}]}
  - {clusterID: e2e-lab, nodeName: e2e-lab-control-plane, role: Leaf, address: 198.18.99.3, listenPort: 51830}
  - {clusterID: e2e-lab, nodeName: e2e-lab-worker, role: Leaf, address: 198.18.99.7}
SITE
  until [[ $(mgmt get netenroll -A -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' | grep -c Enrolled) == 4 ]]; do sleep 2; done
  # The NAT: a robot opens connections to the site's nodes, nothing comes back
  # uninvited, and IP-in-IP does not pass.
  for robot in e2e-lab-control-plane e2e-lab-worker; do
    for node in $HUB e2e-office-worker; do
      docker exec "$robot" sh -c "iptables -I INPUT -i eth0 -s $(ip_of $node) -m conntrack --ctstate NEW -j DROP; iptables -I OUTPUT -o eth0 -d $(ip_of $node) -p 4 -j DROP; iptables -I OUTPUT -o eth0 -d $(ip_of $node) -p tcp --dport 179 -j DROP"
    done
  done
  mgmt get netenroll -A
}

upgrade() {
  local hop
  for hop in v1.32.13 $VERSION; do
    kind load docker-image --name e2e-office "rlark/node-bootstrap:upgrade-$hop" >/dev/null
    "$REPO/extensions/station/hack/upgrade-cluster.sh" "$D/e2e-office.kubeconfig" "$hop" "rlark/node-bootstrap:upgrade-$hop" | grep -v '^ '
    [[ $hop == "$VERSION" ]] && continue
    import e2e-office $(calico_images $CALICO_NEW)
    local t0=$(date +%s)
    calico $CALICO_NEW e2e-office interface=st-wg0 1360 | member e2e-office apply -f - >/dev/null
    calico_ready e2e-office
    echo "$(date +%T) Calico $CALICO_NEW over the tunnel in $(( $(date +%s) - t0 )) s"
    member e2e-office get nodes -o custom-columns='NODE:.metadata.name,INTERNAL:.status.addresses[0].address,CALICO:.metadata.annotations.projectcalico\.org/IPv4Address'
  done
}

installer() {
  rm -rf "$D/images"; mkdir -p "$D/images"
  refs=$(docker exec $HUB ctr -n k8s.io images ls -q | grep -E "kube-proxy:$VERSION|/pause:" | grep -v '@sha256' | sort -u)
  docker exec $HUB ctr -n k8s.io images export - $refs > "$D/images/node.tar"
  docker save "$AGENT_IMAGE" -o "$D/images/station-agent.tar"
  for image in node cni; do docker save --platform linux/amd64 docker.io/calico/$image:$CALICO_NEW -o "$D/images/calico-$image.tar"; done
  KUBE_BIN=$KUBE_DIR/$VERSION CNI_BIN=${CNI_BIN:-/opt/cni/bin} IMAGES="$D/images" "$REPO/extensions/station/bootstrap/build.sh" "$INSTALLER" "$D/pki/installer.tar"
  kind load docker-image --name e2e-lab "$INSTALLER" >/dev/null
}

move() {
  KCP=$D/kcp.kubeconfig CA=$D/pki/ca.crt TAINTS=rlark.io/node-category=robot:NoSchedule \
    "$REPO/extensions/station/hack/migrate-node.sh" "$D/e2e-lab.kubeconfig" "$D/e2e-office.kubeconfig" e2e-office "$1" \
    $HUB:6443 e2e-station-api:18443 "$INSTALLER" rlark.io/node-category=robot rlark.io/robot-id="${1#e2e-lab-}" rlark.io/robot-model=s1u
}

# check: on every robot that is a node of the site cluster, a pod on the pod
# network and one on the host network with the cluster's DNS; each must reach
# a pod on the site entry and resolve a Service name.
check() {
  member e2e-office get nodes -o wide | cut -c1-110
  member e2e-office -n kube-system get pods -l k8s-app=calico-node -o wide --no-headers | awk '{print $1, $2, $3, $7}'
  mgmt get netenroll,nodeadmission -A
  member e2e-office delete pod -l e2e=check --ignore-not-found --wait >/dev/null
  pod() { # name node hostNetwork
    member e2e-office apply -f - >/dev/null <<POD
apiVersion: v1
kind: Pod
metadata: {name: $1, labels: {e2e: check}}
spec:
  nodeName: $2
  hostNetwork: $3
  dnsPolicy: ClusterFirstWithHostNet
  tolerations: [{operator: Exists}]
  terminationGracePeriodSeconds: 0
  containers: [{name: c, image: busybox:1.36, imagePullPolicy: Never, command: [sh, -c, "httpd -f -p 8089 -h /etc"]}]
POD
  }
  pod check-site $HUB false
  local robots robot
  robots=$(member e2e-office get nodes -l rlark.io/node-category=robot -o jsonpath='{.items[*].metadata.name}')
  for robot in $robots; do pod "pod-$robot" "$robot" false; pod "host-$robot" "$robot" true; done
  member e2e-office wait --for=condition=Ready pod -l e2e=check --timeout=120s >/dev/null
  local site
  site=$(member e2e-office get pod check-site -o jsonpath='{.status.podIP}')
  for robot in $robots; do
    for kind in pod host; do
      member e2e-office exec "$kind-$robot" -- sh -c "wget -q -T 5 -O /dev/null http://$site:8089/hostname && echo '$kind-$robot -> pod on the site entry ($site): ok'" || echo "$kind-$robot -> pod on the site entry: FAILED"
      member e2e-office exec "$kind-$robot" -- sh -c "nslookup -timeout=5 kubernetes.default.svc.cluster.local >/dev/null 2>&1 && echo '$kind-$robot resolves a Service name: ok'" || echo "$kind-$robot resolves a Service name: FAILED"
    done
    member e2e-office exec check-site -- sh -c "wget -q -T 5 -O /dev/null http://$(member e2e-office get pod "pod-$robot" -o jsonpath='{.status.podIP}'):8089/hostname && echo 'pod on the site entry -> pod-$robot: ok'" || echo "pod on the site entry -> pod-$robot: FAILED"
  done
  member e2e-office exec check-site -- sh -c 'wget -q -T 5 -O - http://'"$(member e2e-office get pod -n kube-system -l k8s-app=kube-dns -o jsonpath='{.items[0].status.podIP}')"':8181/ready >/dev/null && echo "pod network inside the site: ok"' || echo "pod network inside the site: FAILED"
}

case ${1:-} in
down) down ;;
*) "$@" ;;
esac
