#!/usr/bin/env bash
# Take a kubeadm cluster one Kubernetes minor version up (or to a newer patch),
# node by node, without logging in to any node. The same command serves every
# robot cluster and any number of nodes.
#
#   upgrade-cluster.sh KUBECONFIG VERSION IMAGE
#
#   IMAGE  the upgrade bundle of VERSION (extensions/station/bootstrap/build.sh):
#          kubeadm, kubelet, kubectl and every image the version's control
#          plane and nodes run, so that no node needs the internet.
# Order, as kubeadm prescribes: each control-plane node, then every other node,
# one at a time: etcd snapshot (control plane) -> kubeadm upgrade -> drain ->
# kubelet -> back to its previous scheduling state. It stops at the first
# failed check. What a node replaced is kept on it in
# /var/backups/rlark-kube-upgrade/<old version>/, the snapshot too
# (etcd-snapshot.db). ROLLBACK=yes with the old version as VERSION and the
# image of the failed upgrade takes the worker nodes back, after the control
# plane has been taken back on its node (upgrade.sh, PHASE=rollback).
# Environment: PULL_SECRET (name of an image pull secret in kube-system), NODES.
set -euo pipefail
K=$1 VERSION=$2 IMAGE=$3
k() { kubectl --kubeconfig "$K" "$@"; }
stamp() { echo "$(date +%T) $*"; }
minor() { echo "$1" | sed -E 's/^v1\.([0-9]+)\..*/\1/'; }
started=$(date +%s)
pull=; [[ -z ${PULL_SECRET:-} ]] || pull="imagePullSecrets: [{name: $PULL_SECRET}]"

current=$(k version -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["serverVersion"]["gitVersion"])')
step=$(( $(minor "$VERSION") - $(minor "$current") ))
[[ $step == 0 || $step == 1 ]] || { echo "the cluster is at $current: kubeadm goes one minor version at a time, not to $VERSION" >&2; exit 1; }
nodes="$(k get nodes -l node-role.kubernetes.io/control-plane -o name) $(k get nodes -l '!node-role.kubernetes.io/control-plane' -o name)"
# NODES limits the run to these nodes ("node/a node/b", control plane first):
# to go on after a stop, or to leave out a node that cannot be upgraded yet. A
# kubelet may stay up to three minor versions behind the control plane.
nodes=${NODES:-$nodes}
if [[ -z ${ROLLBACK:-} ]]; then
  for item in $nodes; do
    [[ $(k get "$item" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}') == True ]] || { echo "${item#node/} is not Ready: a node must be Ready before its upgrade" >&2; exit 1; }
  done
fi
stamp "cluster at $current -> $VERSION; order:$(echo $nodes | sed 's#node/# #g')"

# job runs one phase of upgrade.sh on one node and waits for it; the API may
# be away for a while when the phase restarts the control plane.
job() { # node phase
  local node=$1 phase=$2 name status
  name=$(echo "kube-upgrade-$phase-$node" | cut -c1-63 | sed 's/-*$//')
  k -n kube-system delete job "$name" --ignore-not-found >/dev/null
  k apply -f - >/dev/null <<JOB
apiVersion: batch/v1
kind: Job
metadata: {name: $name, namespace: kube-system, labels: {app: rlark-kube-upgrade}}
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 86400
  template:
    metadata: {labels: {app: rlark-kube-upgrade}}
    spec:
      nodeName: $node
      hostNetwork: true
      hostPID: true
      restartPolicy: Never
      tolerations: [{operator: Exists}]
      $pull
      containers:
      - name: upgrade
        image: $IMAGE
        imagePullPolicy: IfNotPresent
        securityContext: {privileged: true}
        env: [{name: PHASE, value: $phase}, {name: KUBE_VERSION, value: $VERSION}]
JOB
  until status=$(k -n kube-system get job "$name" -o jsonpath='{.status.succeeded}/{.status.failed}' 2>/dev/null) && [[ $status == 1/* || $status == */1 ]]; do sleep 2; done
  k -n kube-system logs "job/$name" 2>/dev/null | sed 's/^/    /'
  [[ $status == 1/* ]] || { echo "phase $phase failed on $node" >&2; exit 1; }
}

# Back to VERSION, the version before the last upgrade. The control plane
# first, on its node, where the API cannot be used for it (upgrade.sh,
# PHASE=rollback); then this, for every other node.
if [[ -n ${ROLLBACK:-} ]]; then
  for item in $(k get nodes -l '!node-role.kubernetes.io/control-plane' -o name); do
    node=${item#node/}
    job "$node" rollback
    until [[ $(k get node "$node" -o jsonpath='{.status.nodeInfo.kubeletVersion} {.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null) == "$VERSION True" ]]; do sleep 2; done
    stamp "$node: back at $VERSION and Ready"
  done
  k get nodes -o wide
  exit 0
fi

for item in $nodes; do
  node=${item#node/}
  cordoned=$(k get node "$node" -o jsonpath='{.spec.unschedulable}')
  if k get nodes -l node-role.kubernetes.io/control-plane -o name | grep -qx "node/$node"; then
    k -n kube-system exec "etcd-$node" -- etcdctl --endpoints https://127.0.0.1:2379 --cacert /etc/kubernetes/pki/etcd/ca.crt \
      --cert /etc/kubernetes/pki/etcd/server.crt --key /etc/kubernetes/pki/etcd/server.key \
      snapshot save "/var/lib/etcd/rlark-snapshot-before-$VERSION.db" >/dev/null
    stamp "$node: etcd snapshot saved"
  fi
  t0=$(date +%s)
  job "$node" control
  until k get --raw /readyz >/dev/null 2>&1; do sleep 1; done
  stamp "$node: kubeadm phase done in $(( $(date +%s) - t0 )) s"
  k drain "$node" --ignore-daemonsets --delete-emptydir-data --force --pod-selector='app!=rlark-kube-upgrade' --timeout=180s >/dev/null
  t0=$(date +%s)
  job "$node" kubelet
  until [[ $(k get node "$node" -o jsonpath='{.status.nodeInfo.kubeletVersion} {.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null) == "$VERSION True" ]]; do sleep 2; done
  [[ $cordoned == true ]] || k uncordon "$node" >/dev/null
  stamp "$node: kubelet $VERSION and Ready in $(( $(date +%s) - t0 )) s"
done
k get nodes -o wide
[[ $(k version -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["serverVersion"]["gitVersion"])') == "$VERSION" ]]
stamp "cluster at $VERSION in $(( $(date +%s) - started )) s"
