#!/bin/bash
# Isolated run of hack/upgrade-cluster.sh: a kind cluster of a control-plane
# node and a worker at v1.31 goes to v1.32 and v1.33. A static pod on each
# node stands for what must keep running (a robot's device layer): its
# container must be the same one before and after.
#
#   upgrade.sh bundle VERSION   build the upgrade bundle of VERSION (pulls its images once)
#   upgrade.sh up               the v1.31 cluster with the static pods
#   upgrade.sh to VERSION       load the bundle into the nodes and upgrade
#   upgrade.sh witness          the static pods' container IDs and start times
#   upgrade.sh down
#
# KUBE_DIR holds <version>/{kubeadm,kubelet,kubectl}. Not covered here: cgroup
# v1, a vendor's containerd, Calico (kind uses kindnet), a registry pull.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../../.." && pwd)
D=${E2E_DIR:-$HOME/.cache/station-e2e-upgrade}
KUBE_DIR=${KUBE_DIR:-$REPO/../kube}
NAME=e2e-up
K=$D/kubeconfig
image() { echo "rlark/node-bootstrap:upgrade-$1"; }

bundle() {
  local version=$1 images=$D/images-$1
  mkdir -p "$images"
  refs=$("$KUBE_DIR/$version/kubeadm" config images list --kubernetes-version "$version")
  for ref in $refs; do docker image inspect "$ref" >/dev/null 2>&1 || docker pull -q "$ref"; done
  docker save $refs -o "$images/control-plane.tar"
  KUBE_BIN=$KUBE_DIR/$version CNI_BIN= IMAGES=$images "$REPO/extensions/station/bootstrap/build.sh" "$(image "$version")" "$D/bundle-$version.tar"
}

up() {
  mkdir -p "$D"
  printf 'kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n- role: control-plane\n- role: worker\n' > "$D/kind.yaml"
  kind create cluster --name $NAME --image "${NODE_IMAGE:-kindest/node:v1.31.0}" --config "$D/kind.yaml" --kubeconfig "$K" --wait 120s 2>&1 | tail -1
  pause=$(docker exec $NAME-control-plane ctr -n k8s.io images ls -q | grep '/pause:' | grep -v sha256 | head -1)
  for node in $NAME-control-plane $NAME-worker; do
    docker exec -i "$node" sh -c 'cat > /etc/kubernetes/manifests/witness.yaml' <<POD
apiVersion: v1
kind: Pod
metadata: {name: witness, namespace: default}
spec:
  hostNetwork: true
  containers: [{name: witness, image: $pause, imagePullPolicy: Never}]
POD
  done
  # A setting of one node only, as a workstation's swap handling is: it must survive the upgrades.
  docker exec $NAME-worker sh -c "sed -i '/^maxPods:/d' /var/lib/kubelet/config.yaml && echo 'maxPods: 77' >> /var/lib/kubelet/config.yaml && systemctl restart kubelet"
  until [[ $(kubectl --kubeconfig "$K" get pods -o name 2>/dev/null | grep -c witness) == 2 ]]; do sleep 1; done
  until [[ $(kubectl --kubeconfig "$K" get node $NAME-worker -o jsonpath='{.status.capacity.pods}') == 77 ]]; do sleep 1; done
  kubectl --kubeconfig "$K" wait --for=condition=Ready pod --all --timeout=60s >/dev/null
  witness
}

witness() {
  kubectl --kubeconfig "$K" get nodes -o jsonpath='{range .items[*]}{.metadata.name} {.status.nodeInfo.kubeletVersion} ready={.status.conditions[?(@.type=="Ready")].status} maxPods={.status.capacity.pods}{"\n"}{end}'
  kubectl --kubeconfig "$K" get pods -o jsonpath='{range .items[*]}{.metadata.name} restarts={.status.containerStatuses[0].restartCount} started={.status.containerStatuses[0].state.running.startedAt} id={.status.containerStatuses[0].containerID}{"\n"}{end}' | cut -c1-140
}

to() {
  kind load docker-image --name $NAME "$(image "$1")" >/dev/null
  "$REPO/extensions/station/hack/upgrade-cluster.sh" "$K" "$1" "$(image "$1")"
  witness
}

case ${1:-} in
down) kind delete cluster --name $NAME >/dev/null 2>&1 || true; rm -rf "$D" ;;
*) "$@" ;;
esac
