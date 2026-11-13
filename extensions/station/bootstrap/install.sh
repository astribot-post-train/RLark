#!/bin/sh
# Second half of the node bootstrap; station-bootstrap runs it as a host
# process with the answer of Station API in the environment. Order matters:
# the tunnel comes first, because the node registers with its tunnel address.
#
# Leaves on the machine:
#   /opt/rlark/bin                      kubeadm, kubelet, kubectl, station-gateway
#   rlark-station-gateway.service       the tunnel, started before the kubelet on every boot
#   containerd-fleet.service            a containerd of its own for the kubelet (the machine's docker is not touched)
#   kubelet.service                     reads drop-ins from /etc/kubernetes/kubelet.conf.d
#   /etc/hosts                          the site's names, kept by the tunnel service (lines ending in "# rlark names")
#   /etc/rlark/station/resolv.conf      the site's name servers, likewise, for the kubelet to give to pods
#   rlark-node-keep.timer               after every boot and every ten minutes, what is listed here is as written here (keep.sh)
# and on its Node the annotations rlark.io/node-install-spec and -state (lib.sh).
set -eu
. /var/lib/rlark/bootstrap/lib.sh
SOCKET=/run/containerd-fleet/containerd.sock
# The site settings this node is installed from; what is this node's alone
# (name, tunnel address, labels) is not among them.
SETTINGS="worker api=$JOIN_API_SERVER interface=$TUNNEL_INTERFACE"
command -v containerd >/dev/null || fail "containerd is required; it is installed together with docker"
modprobe wireguard || fail "the kernel has no wireguard module"

# 0. A machine that is a node already leaves its cluster first (RLARK_MIGRATE,
#    set by migrate-node.sh). It keeps its container runtime, and with it the
#    images it has, and its WireGuard key, and with that its tunnel address.
if [ -e /etc/kubernetes/kubelet.conf ]; then
  [ -n "${RLARK_MIGRATE:-}" ] || fail "this machine is already a node (/etc/kubernetes/kubelet.conf exists); moving it is migrate-node.sh"
  [ ! -e /etc/kubernetes/manifests/kube-apiserver.yaml ] || [ "$RLARK_MIGRATE" = control-plane ] ||
    fail "this node is its cluster's control plane; the cluster ends when it leaves, so it moves last and with RLARK_MIGRATE=control-plane"
  SOCKET=$(sed -n 's/.*--container-runtime-endpoint=unix:\/\/\([^ "]*\).*/\1/p' /var/lib/kubelet/kubeadm-flags.env)
  [ -S "$SOCKET" ] || fail "cannot find the node's container runtime socket"
  old=$(dirname "$(systemctl show kubelet -p ExecStart --value | sed -n 's/.*path=\([^ ;]*kubelet\) .*/\1/p')")
  # What makes this machine a node of the old cluster is kept: if it does not
  # become a node of the new one, it goes back by itself (back_to_old_cluster).
  # A control-plane node's etcd is not among it: migrate-node.sh snapshots it.
  OLD=$B/old-cluster
  keep_old_cluster
  if [ -e /var/lib/etcd/rlark-snapshot-before-migrate.db ]; then
    install -d /var/backups/rlark-node-migrate && mv /var/lib/etcd/rlark-snapshot-before-migrate.db /var/backups/rlark-node-migrate/etcd-snapshot.db
    cp -a "$OLD/etc-kubernetes" /var/backups/rlark-node-migrate/
  fi
  systemctl stop kubelet
  "$old/kubeadm" reset --force --cri-socket "unix://$SOCKET" > "$B"/reset.log 2>&1 || { tail -20 "$B"/reset.log >&2; fail "kubeadm reset"; }
  rm -rf /etc/cni/net.d/*
  KEEP_RUNTIME=1
  stamp "left the old cluster; runtime $SOCKET and its images are kept"
  back_to_old_cluster() {
    code=$?
    [ $code != 0 ] || return 0
    trap - EXIT
    echo "$(date +%T) the move failed; back to the old cluster" >&2
    systemctl stop kubelet 2>/dev/null || true
    [ ! -x /opt/rlark/bin/kubeadm ] || /opt/rlark/bin/kubeadm reset --force --cri-socket "unix://$SOCKET" >/dev/null 2>&1 || true
    restore_old_cluster
    echo "$(date +%T) the old kubelet is started with its old identity; what failed is in $B/install.log" >&2
    exit $code
  }
  trap back_to_old_cluster EXIT
fi

install -d /opt/rlark/bin /opt/cni/bin /etc/rlark/station /etc/kubernetes/kubelet.conf.d /etc/containerd-fleet
install -m 755 "$B"/bin/* /opt/rlark/bin/
cp -n "$B"/cni/* /opt/cni/bin/

# 1. The tunnel, as a host service (lib.sh).
tunnel_service
# The peers learn this machine's key from their agents, which ask every 30 s.
tries=0
until station-gateway --config /etc/rlark/station/bootstrap.json --status 2>/dev/null | grep -q '"lastHandshake":"'; do
  tries=$((tries + 1))
  [ $tries -lt 120 ] || fail "no peer has answered through the tunnel after 2 min: station-gateway --config /etc/rlark/station/bootstrap.json --status"
  sleep 1
done
stamp "tunnel up: $TUNNEL_ADDRESS on $TUNNEL_INTERFACE"

# 2. A containerd for the kubelet, with the images a node needs before it
#    can pull anything.
if [ -z "${KEEP_RUNTIME:-}" ]; then own_runtime; else kernel; fi
import_images "$B"/images
stamp "container runtime ready, $(ls "$B"/images/*.tar | wc -l) image archives imported"

# 3. The kubelet (lib.sh). A node joins at the control plane's name, which
#    the tunnel service has written into /etc/hosts from the site's names: the
#    control plane can move to another machine or network without any node's
#    certificates or kubeconfig changing.
kubelet_unit
case ${JOIN_API_SERVER%:*} in *[a-z]*) getent hosts "${JOIN_API_SERVER%:*}" >/dev/null ;; esac ||
  fail "this machine cannot resolve ${JOIN_API_SERVER%:*}, the name of its cluster's control plane: it is not among the names its site network gave it (SiteNetwork, names)"

# 4. Join. The node registers with its tunnel address, its labels and taints.
{
  cat <<JOIN
apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
discovery:
  bootstrapToken:
    apiServerEndpoint: "$JOIN_API_SERVER"
    token: "$JOIN_TOKEN"
    caCertHashes: ["$JOIN_CA_HASH"]
nodeRegistration:
  name: "$NODE_NAME"
  criSocket: unix://$SOCKET
  ignorePreflightErrors: [Swap, FileExisting-conntrack, FileExisting-socat]
  kubeletExtraArgs:
  - {name: node-ip, value: "$TUNNEL_ADDRESS"}
  - {name: node-labels, value: "${NODE_LABELS:+$NODE_LABELS,}station.rlark.io/network=host"}
  taints:
JOIN
  for taint in $(echo "$NODE_TAINTS" | tr ',' ' '); do
    pair=${taint%:*}
    echo "  - {key: \"${pair%%=*}\", value: \"${pair#*=}\", effect: \"${taint##*:}\"}"
  done
} > "$B"/join.yaml
[ -n "$NODE_TAINTS" ] || sed -i 's/^  taints:$/  taints: []/' "$B"/join.yaml
kubeadm join --config "$B"/join.yaml > "$B"/join.log 2>&1 || { tail -20 "$B"/join.log >&2; fail "kubeadm join"; }
stamp "joined $JOIN_API_SERVER as $NODE_NAME"
tries=0
until kubectl --kubeconfig /etc/kubernetes/kubelet.conf get node "$NODE_NAME" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null | grep -q True; do
  tries=$((tries + 1))
  [ $tries -lt 90 ] || { node_install "$SETTINGS" "not Ready 3 min after its join" || true; fail "the node is registered but not Ready after 3 min: kubectl describe node $NODE_NAME"; }
  sleep 2
done
stamp "node Ready"
node_install "$SETTINGS" ok
keep_service "$(unit_of "$SOCKET")"
rm -rf "$B"/images "$B"/join.yaml
echo "Done. Nothing else is needed on this machine."
