#!/bin/bash
# Isolated run of "a factory-fresh machine becomes a node by one command".
# The site is one kind cluster (its control plane is the site entry, a hub);
# the machine is a container that has systemd and containerd and nothing of
# Kubernetes. The installer runs the way the operator's command runs it,
# except that it shares the machine's namespaces by container name, the
# machine being a container without a docker of its own.
#
#   admit.sh up        management plane, site cluster, installer image
#   admit.sh machine   a fresh machine named e2e-robot
#   admit.sh admit     write the NodeAdmission, run the installer, time every step
#   admit.sh reboot    restart the machine, time tunnel and Ready
#   admit.sh down
#
# Needs fs.inotify.max_user_instances >= 1024 and the Station images of
# IMAGES_TAG (hack/e2e/build-local.sh).
set -euo pipefail
export E2E_CLUSTERS="e2e-office=1" IMAGES_TAG=${IMAGES_TAG:-e2e-admit}
HERE=$(cd "$(dirname "$0")" && pwd)
source "$HERE/e2e.sh"
INSTALLER=rlark/node-bootstrap:$TAG
HUB=e2e-office-control-plane
now() { date +%s.%N; }
since() { printf '%.1f s' "$(echo "$(now) - $1" | bc)"; }
wait_for() { local what=$1 t0; t0=$(now); shift; until "$@" >/dev/null 2>&1; do sleep 0.5; done; echo "  $what: $(since "$t0")"; }

admit_up() {
  export STATION_API_ARGS="--bootstrap-ca-file=/pki/ca.crt --bootstrap-image-file=/pki/installer.tar --bootstrap-image-ref=$INSTALLER"
  down; certificates
  create_cluster e2e-office 1
  # What a node of this cluster needs before it can pull: taken from the site's own node.
  mkdir -p "$D/images"
  refs=$(docker exec $HUB ctr -n k8s.io images ls -q | grep -E 'kube-proxy|kindnetd|/pause:' | grep -v '@sha256' | sort -u)
  docker exec $HUB ctr -n k8s.io images export - $refs > "$D/images/node.tar"
  docker save "$AGENT_IMAGE" -o "$D/images/station-agent.tar"
  echo "$refs" | grep '/pause:' | head -1 > "$D/images/pause.ref"
  KUBE_BIN=${KUBE_BIN:-/usr/bin} CNI_BIN=${CNI_BIN:-/opt/cni/bin} IMAGES="$D/images" "$REPO/extensions/station/bootstrap/build.sh" "$INSTALLER" "$D/pki/installer.tar"
  chmod 644 "$D/pki/installer.tar"
  # kind gives its own nodes a /kubelet cgroup; a machine that joins has none, as on a real site.
  member e2e-office -n kube-system get cm kubelet-config -o yaml | sed '/^    cgroupRoot: /d' | member e2e-office apply -f - >/dev/null
  plane; agents e2e-office; enable e2e-office
  mgmt apply -f - <<SITE
apiVersion: station.rlark.io/v1alpha1
kind: SiteNetwork
metadata: {name: default, namespace: station-system}
spec:
  mode: Tunnel
  pool: 198.18.99.0/24
  sites:
  - {name: office, clusters: [e2e-office], mesh: true}
  nodes:
  - clusterID: e2e-office
    nodeName: $HUB
    role: Hub
    address: 198.18.99.1
    endpoints: [{address: "$(ip_of $HUB):51820", scope: Site}]
SITE
  wait_for "site entry enrolled" sh -c "kubectl --kubeconfig $D/kcp.kubeconfig -n e2e-office get netenroll $HUB -o jsonpath='{.status.phase}' | grep -q Enrolled"
}

# machine starts a container that stands for a vendor image: systemd, a
# containerd, no Kubernetes binaries, no units of ours.
machine() {
  docker rm -f e2e-robot >/dev/null 2>&1 || true
  docker run -d --name e2e-robot --hostname e2e-robot --network kind --privileged --cgroupns=private \
    --tmpfs /run --tmpfs /tmp -v /var -v /lib/modules:/lib/modules:ro -v /boot:/boot:ro "$NODE_IMAGE" >/dev/null
  wait_for "machine booted" docker exec e2e-robot systemctl is-active containerd
  docker exec e2e-robot sh -c 'systemctl disable --now kubelet >/dev/null 2>&1; rm -rf /etc/systemd/system/kubelet.service* /usr/lib/systemd/system/kubelet.service* /etc/kubernetes /var/lib/kubelet /opt/cni/bin/*; rm -f /usr/bin/kubeadm /usr/bin/kubelet /usr/bin/kubectl; systemctl daemon-reload'
}

admit() {
  local node=${1:-e2e-robot} code t0
  code=$(head -c 512 /dev/urandom | tr -dc 'a-z0-9' | cut -c1-8)
  mgmt -n e2e-office delete nodeadmission "$node" --ignore-not-found >/dev/null
  mgmt apply -f - >/dev/null <<ADMISSION
apiVersion: station.rlark.io/v1alpha1
kind: NodeAdmission
metadata: {name: $node, namespace: e2e-office}
spec:
  clusterID: e2e-office
  codeHash: $(printf %s "$code" | sha256sum | cut -d' ' -f1)
  expiresAt: "$(date -u -d '+2 hours' +%Y-%m-%dT%H:%M:%SZ)"
  apiServer: $HUB:6443
  labels: {rlark.io/node-category: robot, rlark.io/robot-id: s1u-99, rlark.io/robot-model: s1u}
  taints: ["rlark.io/node-category=robot:NoSchedule"]
ADMISSION
  echo "the operator's command: curl -s https://e2e-station-api:18443/b/$code | sudo sh"
  # The script and the archive as the machine would fetch them.
  docker exec e2e-robot curl -fsS --cacert /dev/null -k "https://$(ip_of e2e-station-api):18443/b/$code" | tee "$D/bootstrap.sh" | grep -c 'docker run' >/dev/null
  t0=$(now)
  size=$(docker exec e2e-robot sh -c "curl -fsSk https://$(ip_of e2e-station-api):18443/v1/bootstrap/image | wc -c")
  echo "  installer archive: $size bytes fetched by the machine in $(since "$t0")"
  t0=$(now)
  docker run --rm --privileged --net=container:e2e-robot --pid=container:e2e-robot -v "$D/pki/ca.crt:/ca.crt:ro" \
    -e RLARK_SERVER=https://e2e-station-api:18443 -e RLARK_CODE="$code" "$INSTALLER" 2>&1 | sed 's/^/  | /'
  echo "  installer finished: $(since "$t0")"
  wait_for "agent pod running on the node" sh -c "kubectl --kubeconfig $D/e2e-office.kubeconfig -n station-system get pod -l app=station-agent-host --field-selector spec.nodeName=$node -o jsonpath='{.items[0].status.phase}' | grep -q Running"
  wait_for "site-node identity Bound" sh -c "kubectl --kubeconfig $D/kcp.kubeconfig -n e2e-office get stations -o jsonpath='{range .items[*]}{.spec.binding.nodeName}={.status.phase} {end}' | grep -q '$node=Bound'"
  wait_for "enrolled under its own identity" sh -c "kubectl --kubeconfig $D/kcp.kubeconfig -n e2e-office get netenroll $node -o jsonpath='{.status.phase}' | grep -q Enrolled"
  wait_for "admission Joined" sh -c "kubectl --kubeconfig $D/kcp.kubeconfig -n e2e-office get nodeadmission $node -o jsonpath='{.status.phase}' | grep -q Joined"
  echo "  total from the command to Joined: $(since "$t0")"
  report "$node"
}

report() {
  local node=$1 address
  address=$(mgmt -n e2e-office get nodeadmission "$node" -o jsonpath='{.status.address}')
  member e2e-office get node "$node" -o jsonpath='{.metadata.name} internalIP={.status.addresses[?(@.type=="InternalIP")].address} taints={.spec.taints[*].key}{"\n"}'
  member e2e-office get node "$node" --show-labels --no-headers | tr ',' '\n' | grep -E 'rlark|station' | tr '\n' ' '; echo
  mgmt -n e2e-office get stations,netenroll,nodeadmission
  # The node images have no ping: a TCP connection each way through the tunnel.
  docker exec $HUB bash -c "timeout 3 bash -c 'echo > /dev/tcp/$address/10250' && echo 'site entry -> node kubelet through the tunnel: ok'"
  docker exec e2e-robot bash -c "timeout 3 bash -c 'echo > /dev/tcp/198.18.99.1/6443' && echo 'node -> site entry through the tunnel: ok'"
}

reboot() {
  local node=${1:-e2e-robot} t0
  docker stop -t 5 e2e-robot >/dev/null; sleep 20
  t0=$(now); docker start e2e-robot >/dev/null
  wait_for "tunnel answers after power-on" docker exec $HUB bash -c "timeout 1 bash -c 'echo > /dev/tcp/$(mgmt -n e2e-office get nodeadmission "$node" -o jsonpath='{.status.address}')/22' 2>&1 | grep -q refused || timeout 1 bash -c 'echo > /dev/tcp/$(mgmt -n e2e-office get nodeadmission "$node" -o jsonpath='{.status.address}')/10250'"
  wait_for "node Ready again (counted from the tunnel)" sh -c "kubectl --kubeconfig $D/e2e-office.kubeconfig get node $node -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].lastHeartbeatTime}' | xargs -I{} date -d {} +%s | awk -v t=${t0%.*} '\$1>=t{ok=1} END{exit !ok}'"
  echo "  power-on to Ready heartbeat: $(since "$t0")"
}

installer() {
  KUBE_BIN=${KUBE_BIN:-/usr/bin} CNI_BIN=${CNI_BIN:-/opt/cni/bin} IMAGES="$D/images" "$REPO/extensions/station/bootstrap/build.sh" "$INSTALLER" "$D/pki/installer.tar"
}

case ${1:-} in
up) admit_up ;;
down) docker rm -f e2e-robot >/dev/null 2>&1 || true; down ;;
*) "$@" ;;
esac
