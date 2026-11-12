#!/bin/sh
# Upgrades this node's Kubernetes by one minor version; station-bootstrap runs
# it as a host process from a Job that upgrade-cluster.sh pins to the node.
#   PHASE=control   import the version's images, then "kubeadm upgrade apply"
#                   on a control-plane node or "kubeadm upgrade node" elsewhere
#   PHASE=kubelet   replace kubelet and kubectl and restart the kubelet
# The node's own layout is read from the node: where its kubelet binary is and
# which containerd it uses. What it replaces is kept under /var/backups.
set -eu
B=/var/lib/rlark/bootstrap
stamp() { echo "$(date +%T) $*"; }
fail() { echo "$(date +%T) FAILED: $*" >&2; exit 1; }
BIN=$(dirname "$(systemctl show kubelet -p ExecStart --value | sed -n 's/.*path=\([^ ;]*kubelet\) .*/\1/p')")
[ -x "$BIN/kubelet" ] || fail "cannot find the kubelet binary of kubelet.service"
SOCKET=$(sed -n 's/.*--container-runtime-endpoint=unix:\/\/\([^ "]*\).*/\1/p' /var/lib/kubelet/kubeadm-flags.env)
[ -S "$SOCKET" ] || fail "cannot find the node's container runtime socket"
from=$("$BIN/kubelet" --version | awk '{print $2}')
keep=/var/backups/rlark-kube-upgrade/$from
install -d "$keep"
export PATH="$BIN:$PATH"

case $PHASE in
control)
  for archive in "$B"/images/*.tar; do
    ctr -a "$SOCKET" -n k8s.io images import "$archive" >/dev/null || fail "import $archive"
  done
  stamp "images of $KUBE_VERSION imported"
  [ -e "$keep/kubeadm" ] || cp -a "$BIN/kubeadm" "$keep/kubeadm"
  [ -d "$keep/etc-kubernetes" ] || cp -a /etc/kubernetes "$keep/etc-kubernetes"
  # What this node's kubelet configuration has that the cluster's does not is
  # carried across: kubeadm writes the file anew from the cluster's.
  patches=
  if [ ! -e "$keep/kubelet-overrides.yaml" ]; then
    kubectl --kubeconfig /etc/kubernetes/kubelet.conf -n kube-system get configmap kubelet-config -o jsonpath='{.data.kubelet}' > "$keep/cluster-kubelet.yaml"
    "$B/station-bootstrap" kubelet-overrides /var/lib/kubelet/config.yaml "$keep/cluster-kubelet.yaml" > "$keep/kubelet-overrides.yaml"
  fi
  if [ -s "$keep/kubelet-overrides.yaml" ]; then
    install -d "$keep/patches"
    cp "$keep/kubelet-overrides.yaml" "$keep/patches/kubeletconfiguration+merge.yaml"
    patches="--patches $keep/patches"
    stamp "kept from this node's kubelet configuration: $(grep -v -E '^(apiVersion|kind):' "$keep/kubelet-overrides.yaml" | grep -E '^[a-zA-Z]' | cut -d: -f1 | tr '\n' ' ')"
  fi
  install -m 755 "$B/bin/kubeadm" "$BIN/kubeadm"
  if [ -e /etc/kubernetes/manifests/kube-apiserver.yaml ]; then
    kubeadm upgrade apply "$KUBE_VERSION" --yes $patches > "$keep/upgrade-apply.log" 2>&1 || { tail -25 "$keep/upgrade-apply.log" >&2; fail "kubeadm upgrade apply"; }
    stamp "control plane at $KUBE_VERSION"
  else
    kubeadm upgrade node $patches > "$keep/upgrade-node.log" 2>&1 || { tail -25 "$keep/upgrade-node.log" >&2; fail "kubeadm upgrade node"; }
    stamp "node configuration at $KUBE_VERSION"
  fi
  ;;
kubelet)
  [ -e "$keep/kubelet" ] || cp -a "$BIN/kubelet" "$BIN/kubectl" "$keep/"
  install -m 755 "$B/bin/kubelet" "$BIN/kubelet.new" && mv "$BIN/kubelet.new" "$BIN/kubelet"
  install -m 755 "$B/bin/kubectl" "$BIN/kubectl.new" && mv "$BIN/kubectl.new" "$BIN/kubectl"
  systemctl daemon-reload
  systemctl restart kubelet
  tries=0
  until curl -fsS -m 2 http://127.0.0.1:10248/healthz >/dev/null 2>&1; do
    tries=$((tries + 1))
    [ $tries -lt 90 ] || fail "the kubelet is not healthy 3 min after its restart: journalctl -u kubelet; the old binaries are in $keep"
    sleep 2
  done
  stamp "kubelet $("$BIN/kubelet" --version | awk '{print $2}') healthy"
  ;;
*) fail "PHASE must be control or kubelet" ;;
esac
