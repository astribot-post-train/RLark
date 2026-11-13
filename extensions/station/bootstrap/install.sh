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
set -eu
B=/var/lib/rlark/bootstrap
SOCKET=/run/containerd-fleet/containerd.sock
started=$(date +%s)
stamp() { echo "$(date +%T) +$(( $(date +%s) - started ))s $*"; }
fail() { echo "$(date +%T) FAILED: $*" >&2; exit 1; }
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
  rm -rf "$OLD" && install -d "$OLD/kubelet" "$OLD/units"
  cp -a /etc/kubernetes "$OLD/etc-kubernetes"
  cp -a /var/lib/kubelet/config.yaml /var/lib/kubelet/kubeadm-flags.env /var/lib/kubelet/pki "$OLD/kubelet/"
  [ ! -e /etc/systemd/system/kubelet.service ] || cp -a /etc/systemd/system/kubelet.service "$OLD/units/"
  [ ! -d /etc/systemd/system/kubelet.service.d ] || cp -a /etc/systemd/system/kubelet.service.d "$OLD/units/"
  [ ! -d /etc/cni/net.d ] || cp -a /etc/cni/net.d "$OLD/cni-net.d"
  systemctl is-enabled rlark-station-gateway >/dev/null 2>&1 && touch "$OLD/had-host-gateway"
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
    rm -f /etc/systemd/system/kubelet.service /etc/systemd/system/kubelet.service.d/zz-rlark.conf
    [ ! -e "$OLD/units/kubelet.service" ] || cp -a "$OLD/units/kubelet.service" /etc/systemd/system/
    [ ! -d "$OLD/units/kubelet.service.d" ] || { rm -rf /etc/systemd/system/kubelet.service.d && cp -a "$OLD/units/kubelet.service.d" /etc/systemd/system/; }
    rm -rf /etc/kubernetes && cp -a "$OLD/etc-kubernetes" /etc/kubernetes
    install -d /var/lib/kubelet && cp -a "$OLD/kubelet/." /var/lib/kubelet/
    [ ! -d "$OLD/cni-net.d" ] || { rm -rf /etc/cni/net.d && cp -a "$OLD/cni-net.d" /etc/cni/net.d; }
    # The old cluster's own gateway pod takes the tunnel again.
    [ -e "$OLD/had-host-gateway" ] || systemctl disable --now rlark-station-gateway >/dev/null 2>&1 || true
    systemctl daemon-reload
    systemctl start kubelet
    echo "$(date +%T) the old kubelet is started with its old identity; what failed is in $B/install.log" >&2
    exit $code
  }
  trap back_to_old_cluster EXIT
fi

install -d /opt/rlark/bin /opt/cni/bin /etc/rlark/station /etc/kubernetes/kubelet.conf.d /etc/containerd-fleet
install -m 755 "$B"/bin/* /opt/rlark/bin/
cp -n "$B"/cni/* /opt/cni/bin/

# 1. The tunnel, as a host service. It applies the configuration the agent
#    stores once the node has enrolled, the last one it applied when the
#    machine boots, and the one received now until then.
cat > /etc/systemd/system/rlark-station-gateway.service <<UNIT
[Unit]
Description=RLark site-node tunnel
After=network-online.target
Wants=network-online.target
Before=kubelet.service

[Service]
ExecStartPre=/bin/sh -c 'mkdir -p /run/station-network /run/station-observation && chown 65532 /run/station-network'
ExecStart=/opt/rlark/bin/station-gateway --config /etc/rlark/station/bootstrap.json --computed-config /run/station-network/config.json --identity-file /run/station-observation/identity.json --observation-file /run/station-observation/wireguard.json --prune-routes
ExecStartPost=/bin/sh -c 'for i in \$(seq 100); do ip -4 addr show dev $TUNNEL_INTERFACE 2>/dev/null | grep -q inet && exit 0; sleep 0.2; done; exit 1'
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable rlark-station-gateway >/dev/null 2>&1
systemctl restart rlark-station-gateway || fail "the tunnel service did not start: journalctl -u rlark-station-gateway"
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
if [ -z "${KEEP_RUNTIME:-}" ]; then
containerd config default | sed -e 's#/var/lib/containerd#/var/lib/containerd-fleet#g' -e 's#/run/containerd#/run/containerd-fleet#g' \
  -e 's#SystemdCgroup = false#SystemdCgroup = true#' -e "s#registry.k8s.io/pause:[0-9.]*#$(cat "$B"/images/pause.ref)#" \
  -e "s#^\\( *config_path = \\).*#\\1'/etc/containerd-fleet/certs.d'#" \
  > /etc/containerd-fleet/config.toml
[ ! -d "$B"/certs.d ] || cp -r "$B"/certs.d /etc/containerd-fleet/
cat > /etc/systemd/system/containerd-fleet.service <<UNIT
[Unit]
Description=containerd for the RLark kubelet
After=network.target

[Service]
ExecStart=$(command -v containerd) --config /etc/containerd-fleet/config.toml
Type=notify
Delegate=yes
KillMode=process
Restart=always
RestartSec=5
LimitNOFILE=1048576
TasksMax=infinity

[Install]
WantedBy=multi-user.target
UNIT
modprobe overlay
modprobe br_netfilter
printf '%s\n' overlay br_netfilter wireguard > /etc/modules-load.d/rlark.conf
printf '%s\n' 'net.ipv4.ip_forward = 1' 'net.bridge.bridge-nf-call-iptables = 1' > /etc/sysctl.d/90-rlark.conf
sysctl -q -p /etc/sysctl.d/90-rlark.conf
systemctl daemon-reload
systemctl enable --now containerd-fleet >/dev/null 2>&1 || fail "containerd-fleet did not start: journalctl -u containerd-fleet"
else
modprobe overlay
modprobe br_netfilter
fi
for archive in "$B"/images/*.tar; do
  ctr -a $SOCKET -n k8s.io images import "$archive" >/dev/null || fail "import $archive"
done
stamp "container runtime ready, $(ls "$B"/images/*.tar | wc -l) image archives imported"

# 3. The kubelet. Swap stays on for the machine's own software; pods get none.
cat > /etc/kubernetes/kubelet.conf.d/10-rlark.conf <<CONF
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
failSwapOn: false
memorySwap:
  swapBehavior: NoSwap
CONF
cat > /etc/systemd/system/kubelet.service <<UNIT
[Unit]
Description=RLark kubelet
After=network-online.target containerd-fleet.service rlark-station-gateway.service
Wants=network-online.target rlark-station-gateway.service

[Service]
Environment="KUBELET_KUBECONFIG_ARGS=--bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf"
Environment="KUBELET_CONFIG_ARGS=--config=/var/lib/kubelet/config.yaml --config-dir=/etc/kubernetes/kubelet.conf.d"
EnvironmentFile=-/var/lib/kubelet/kubeadm-flags.env
ExecStart=/opt/rlark/bin/kubelet \$KUBELET_KUBECONFIG_ARGS \$KUBELET_CONFIG_ARGS \$KUBELET_KUBEADM_ARGS
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
# A distribution's or an earlier installation's drop-in may set ExecStart; this one is read last.
install -d /etc/systemd/system/kubelet.service.d
cat > /etc/systemd/system/kubelet.service.d/zz-rlark.conf <<UNIT
[Service]
Environment="KUBELET_KUBECONFIG_ARGS=--bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf"
Environment="KUBELET_CONFIG_ARGS=--config=/var/lib/kubelet/config.yaml --config-dir=/etc/kubernetes/kubelet.conf.d"
EnvironmentFile=-/var/lib/kubelet/kubeadm-flags.env
ExecStart=
ExecStart=/opt/rlark/bin/kubelet \$KUBELET_KUBECONFIG_ARGS \$KUBELET_CONFIG_ARGS \$KUBELET_KUBEADM_ARGS
UNIT
systemctl daemon-reload
systemctl enable kubelet >/dev/null 2>&1
# A node class's own profile (a robot's CPU partition) derives its drop-in
# from the kubelet's unit, which has just changed.
[ ! -x /usr/local/sbin/fleet-node-profile ] || /usr/local/sbin/fleet-node-profile install >> "$B"/node-profile.log 2>&1 || fail "the node profile did not accept the new kubelet unit: $B/node-profile.log"

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
  [ $tries -lt 90 ] || fail "the node is registered but not Ready after 3 min: kubectl describe node $NODE_NAME"
  sleep 2
done
stamp "node Ready"
rm -rf "$B"/images "$B"/join.yaml
echo "Done. Nothing else is needed on this machine."
