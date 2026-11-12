#!/bin/sh
# Upgrades this node's Kubernetes by one minor version; station-bootstrap runs
# it as a host process from a Job that upgrade-cluster.sh pins to the node.
#   PHASE=control   import the version's images, then "kubeadm upgrade apply"
#                   on a control-plane node or "kubeadm upgrade node" elsewhere
#   PHASE=kubelet   replace kubelet and kubectl and restart the kubelet
#   PHASE=rollback  put the node back to KUBE_VERSION, the version it had
#                   before its last upgrade: binaries, configuration, and on a
#                   control-plane node the static pods and etcd's content of
#                   the snapshot taken before that upgrade. What was written to
#                   the cluster since the snapshot is lost. On a control-plane
#                   node the API is down, so the image is started on the node:
#                   docker run --rm --privileged --pid=host --net=host -e PHASE=rollback -e KUBE_VERSION=... IMAGE
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
[ "$PHASE" = rollback ] || install -d "$keep"
export PATH="$BIN:$PATH"
# A kubelet checks for swap only when it starts. On a node where swap was
# turned on under a running kubelet that is not configured for it, no kubelet
# starts again, the old one included: nothing is changed on such a node.
if [ "$PHASE" != rollback ] && [ "$(wc -l < /proc/swaps)" -gt 1 ] &&
  ! grep -rqs -E '^failSwapOn: *false' /var/lib/kubelet/config.yaml /etc/kubernetes/kubelet.conf.d &&
  ! grep -qs -e '--fail-swap-on=false' /var/lib/kubelet/kubeadm-flags.env /etc/default/kubelet /etc/sysconfig/kubelet; then
  fail "swap is on ($(sed -n 2p /proc/swaps | cut -d' ' -f1)) and this node's kubelet is not configured for it (failSwapOn: false): a restarted kubelet would exit at once. Turn the swap off or declare failSwapOn: false for the node, then upgrade it"
fi

case $PHASE in
control)
  for archive in "$B"/images/*.tar; do
    ctr -a "$SOCKET" -n k8s.io images import "$archive" >/dev/null || fail "import $archive"
  done
  stamp "images of $KUBE_VERSION imported"
  [ -e "$keep/kubeadm" ] || cp -a "$BIN/kubeadm" "$keep/kubeadm"
  [ -d "$keep/etc-kubernetes" ] || cp -a /etc/kubernetes "$keep/etc-kubernetes"
  [ -d "$keep/kubelet" ] || { install -d "$keep/kubelet" && cp -a /var/lib/kubelet/config.yaml /var/lib/kubelet/kubeadm-flags.env "$keep/kubelet/"; }
  # upgrade-cluster.sh took this snapshot just before it started this phase.
  [ ! -e "/var/lib/etcd/rlark-snapshot-before-$KUBE_VERSION.db" ] || mv "/var/lib/etcd/rlark-snapshot-before-$KUBE_VERSION.db" "$keep/etcd-snapshot.db"
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
    # kubeadm reads the node's runtime socket from an annotation of the Node,
    # which a node that was not joined by kubeadm join alone may lack.
    node=$(cat /var/lib/kubelet/kubeadm-flags.env /etc/default/kubelet /etc/sysconfig/kubelet 2>/dev/null | sed -n 's/.*--hostname-override=\([^ "]*\).*/\1/p' | head -1)
    [ -n "$node" ] || node=$(hostname | tr '[:upper:]' '[:lower:]')
    [ -n "$(kubectl --kubeconfig /etc/kubernetes/kubelet.conf get node "$node" -o jsonpath='{.metadata.annotations.kubeadm\.alpha\.kubernetes\.io/cri-socket}')" ] ||
      kubectl --kubeconfig /etc/kubernetes/kubelet.conf annotate node "$node" "kubeadm.alpha.kubernetes.io/cri-socket=unix://$SOCKET" >/dev/null
    kubeadm upgrade node $patches > "$keep/upgrade-node.log" 2>&1 || { tail -25 "$keep/upgrade-node.log" >&2; fail "kubeadm upgrade node"; }
    stamp "node configuration at $KUBE_VERSION"
  fi
  ;;
kubelet)
  [ -e "$keep/bin/kubelet" ] || { install -d "$keep/bin" && cp -a "$BIN/kubelet" "$BIN/kubectl" "$keep/bin/"; }
  install -m 755 "$B/bin/kubelet" "$BIN/kubelet.new" && mv "$BIN/kubelet.new" "$BIN/kubelet"
  install -m 755 "$B/bin/kubectl" "$BIN/kubectl.new" && mv "$BIN/kubectl.new" "$BIN/kubectl"
  systemctl daemon-reload
  systemctl restart kubelet
  tries=0
  until curl -fsS -m 2 http://127.0.0.1:10248/healthz >/dev/null 2>&1; do
    tries=$((tries + 1))
    if [ $tries -ge 60 ]; then
      # Without a kubelet nothing reaches this node through the cluster, so
      # the node goes back to what it ran before, by itself.
      journalctl -u kubelet --no-pager --since "-3min" > "$keep/kubelet-failed.log" 2>&1 || true
      grep -v -E 'Failed to ensure lease|Unable to register node|node .* not found' "$keep/kubelet-failed.log" | tail -15 >&2
      install -m 755 "$keep/bin/kubelet" "$BIN/kubelet.old" && mv "$BIN/kubelet.old" "$BIN/kubelet"
      install -m 755 "$keep/bin/kubectl" "$BIN/kubectl.old" && mv "$BIN/kubectl.old" "$BIN/kubectl"
      [ ! -d "$keep/kubelet" ] || cp -a "$keep/kubelet/config.yaml" "$keep/kubelet/kubeadm-flags.env" /var/lib/kubelet/
      systemctl restart kubelet
      fail "the kubelet of $KUBE_VERSION was not healthy 2 min after its start; the node is back on $from, the kubelet's log is $keep/kubelet-failed.log"
    fi
    sleep 2
  done
  stamp "kubelet $("$BIN/kubelet" --version | awk '{print $2}') healthy"
  ;;
rollback)
  keep=/var/backups/rlark-kube-upgrade/$KUBE_VERSION
  [ -d "$keep/etc-kubernetes" ] || { stamp "nothing of $KUBE_VERSION is kept under $keep: this node was not upgraded from it"; exit 0; }
  if [ -e "$keep/etc-kubernetes/manifests/kube-apiserver.yaml" ]; then
    [ -e "$keep/etcd-snapshot.db" ] || fail "no etcd snapshot of $KUBE_VERSION in $keep"
    # The kubelet stops the control plane when its manifests go.
    aside=/var/backups/rlark-kube-upgrade/failed-$from-$(date +%Y%m%d%H%M%S)
    install -d "$aside"
    mv /etc/kubernetes/manifests "$aside/manifests" && install -d /etc/kubernetes/manifests
    tries=0
    while ss -ltn | grep -q -E ':(2379|2380) '; do
      tries=$((tries + 1))
      [ $tries -lt 60 ] || fail "etcd is still running 2 min after its manifest was removed"
      sleep 2
    done
    systemctl stop kubelet
    [ ! -d /var/lib/etcd ] || mv /var/lib/etcd "$aside/etcd"
    manifest=$keep/etc-kubernetes/manifests/etcd.yaml
    # The revision is raised past anything a client has seen: a kubelet that
    # holds a later resource version than the snapshot's would otherwise wait
    # for it for ever, and the other nodes are taken back through their kubelets.
    flag() { sed -n "s/^ *- --$1=//p" "$manifest"; }
    ctr -a "$SOCKET" -n k8s.io run --rm --mount "type=bind,src=/var/lib,dst=/var/lib,options=rbind:rw" --mount "type=bind,src=$keep,dst=/keep,options=rbind:ro" \
      "$(sed -n 's/^ *image: //p' "$manifest")" rlark-etcd-restore etcdctl snapshot restore /keep/etcd-snapshot.db --data-dir /var/lib/etcd \
      --bump-revision 1000000000 --mark-compacted \
      --name "$(flag name)" --initial-cluster "$(flag initial-cluster)" --initial-advertise-peer-urls "$(flag initial-advertise-peer-urls)" > "$aside/etcd-restore.log" 2>&1 ||
      { tail -20 "$aside/etcd-restore.log" >&2; fail "etcd restore; the data directory of $from is $aside/etcd"; }
    stamp "etcd holds the snapshot taken before the upgrade from $KUBE_VERSION"
    cp -a /etc/kubernetes "$aside/etc-kubernetes"
    rm -rf /etc/kubernetes && cp -a "$keep/etc-kubernetes" /etc/kubernetes
    install -m 755 "$keep/kubeadm" "$BIN/kubeadm"
  else
    systemctl stop kubelet
  fi
  cp -a "$keep/kubelet/config.yaml" "$keep/kubelet/kubeadm-flags.env" /var/lib/kubelet/
  if [ -e "$keep/bin/kubelet" ]; then
    install -m 755 "$keep/bin/kubelet" "$BIN/kubelet.old" && mv "$BIN/kubelet.old" "$BIN/kubelet"
    install -m 755 "$keep/bin/kubectl" "$BIN/kubectl.old" && mv "$BIN/kubectl.old" "$BIN/kubectl"
  fi
  systemctl start kubelet
  tries=0
  until curl -fsS -m 2 http://127.0.0.1:10248/healthz >/dev/null 2>&1; do
    tries=$((tries + 1))
    [ $tries -lt 90 ] || fail "the kubelet is not healthy 3 min after the rollback: journalctl -u kubelet"
    sleep 2
  done
  stamp "node back at $("$BIN/kubelet" --version | awk '{print $2}')"
  ;;
*) fail "PHASE must be control, kubelet or rollback" ;;
esac
