# What install.sh (a machine becomes a worker) and control-plane.sh (a machine
# becomes a control plane) do the same way. Sourced, not run.
B=/var/lib/rlark/bootstrap
started=$(date +%s)
stamp() { echo "$(date +%T) +$(( $(date +%s) - started ))s $*"; }
fail() { echo "$(date +%T) FAILED: $*" >&2; exit 1; }

# hosts_name ADDRESS NAME: /etc/hosts holds the one line
# "ADDRESS NAME # rlark control-plane"; without an address it holds none.
# This is a control plane's own name on its own machine, which must resolve
# there before any site network declares it and whatever one declares later.
# The names a site declares are the tunnel service's lines ("# rlark names").
# The file keeps its place and is never empty in between: it is written over
# from its start, then cut to the new length.
hosts_name() {
  # Where the line stands among the others is not this function's: the
  # tunnel service puts its own lines last whenever it writes them.
  [ "$(grep ' # rlark control-plane$' /etc/hosts)" != "${1:+$1 $2 # rlark control-plane}" ] || return 0
  { grep -v ' # rlark control-plane$' /etc/hosts || true; [ -z "$1" ] || printf '%s %s # rlark control-plane\n' "$1" "$2"; } > "$B/hosts"
  dd if="$B/hosts" of=/etc/hosts conv=notrunc 2>/dev/null && truncate -s "$(wc -c < "$B/hosts")" /etc/hosts
  changed=hosts
}

# ctr_of SOCKET: the ctr beside the containerd that serves the socket. A
# machine's containerd need not be in the PATH, nor be the only one.
ctr_of() {
  echo "$(dirname "$(readlink "/proc/$(ss -Hxlp | awk -v s="$(readlink -f "$1")" '$5 == s' | sed -n 's/.*pid=\([0-9]*\).*/\1/p')/exe")")/ctr"
}

# keep_old_cluster: what makes this machine a node of the cluster it is in,
# kept in $OLD before it leaves; restore_old_cluster puts it back and starts
# the old kubelet with its old identity.
keep_old_cluster() {
  rm -rf "$OLD" && install -d "$OLD/kubelet" "$OLD/units"
  cp -a /etc/kubernetes "$OLD/etc-kubernetes"
  cp -a /var/lib/kubelet/config.yaml /var/lib/kubelet/kubeadm-flags.env /var/lib/kubelet/pki "$OLD/kubelet/"
  [ ! -e /etc/systemd/system/kubelet.service ] || cp -a /etc/systemd/system/kubelet.service "$OLD/units/"
  [ ! -d /etc/systemd/system/kubelet.service.d ] || cp -a /etc/systemd/system/kubelet.service.d "$OLD/units/"
  [ ! -d /etc/cni/net.d ] || cp -a /etc/cni/net.d "$OLD/cni-net.d"
  ! systemctl is-enabled rlark-station-gateway >/dev/null 2>&1 || touch "$OLD/had-host-gateway"
}
restore_old_cluster() {
  rm -f /etc/systemd/system/kubelet.service /etc/systemd/system/kubelet.service.d/zz-rlark.conf
  [ ! -e "$OLD/units/kubelet.service" ] || cp -a "$OLD/units/kubelet.service" /etc/systemd/system/
  [ ! -d "$OLD/units/kubelet.service.d" ] || { rm -rf /etc/systemd/system/kubelet.service.d && cp -a "$OLD/units/kubelet.service.d" /etc/systemd/system/; }
  rm -rf /etc/kubernetes && cp -a "$OLD/etc-kubernetes" /etc/kubernetes
  install -d /var/lib/kubelet && cp -a "$OLD/kubelet/." /var/lib/kubelet/
  # The directory itself stays: the container runtime watches it, and would
  # never see what is written into a new one in its place.
  [ ! -d "$OLD/cni-net.d" ] || { install -d /etc/cni/net.d && rm -rf /etc/cni/net.d/* && cp -a "$OLD/cni-net.d/." /etc/cni/net.d/; }
  # The old cluster's own gateway pod takes the tunnel again.
  [ -e "$OLD/had-host-gateway" ] || systemctl disable --now rlark-station-gateway >/dev/null 2>&1 || true
  systemctl daemon-reload
  systemctl start kubelet
}

# tunnel_service: the site tunnel as a host service, started before the
# kubelet on every boot. It applies the configuration the agent stores once
# the node has enrolled, the last one it applied when the machine boots or
# when the pod that ran the tunnel is gone, and the administrator's document
# until then; the names its document holds it keeps in /etc/hosts, the name
# servers (the site's, else the machine's own) in
# /etc/rlark/station/resolv.conf. A service that is running as written is left alone.
tunnel_service() {
  install -d /etc/rlark/station
  cat > "$B/rlark-station-gateway.service" <<UNIT
[Unit]
Description=RLark site-node tunnel
After=network-online.target
Wants=network-online.target
Before=kubelet.service

[Service]
ExecStartPre=/bin/sh -c 'mkdir -p /run/station-network /run/station-observation && chown 65532 /run/station-network'
ExecStart=/opt/rlark/bin/station-gateway --config /etc/rlark/station/bootstrap.json --computed-config /run/station-network/config.json --identity-file /run/station-observation/identity.json --observation-file /run/station-observation/wireguard.json --prune-routes --hosts-file /etc/hosts --resolv-file /etc/rlark/station/resolv.conf
ExecStartPost=/bin/sh -c 'for i in \$(seq 100); do ip -4 addr show dev $TUNNEL_INTERFACE 2>/dev/null | grep -q inet && exit 0; sleep 0.2; done; exit 1'
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT
  if cmp -s "$B/rlark-station-gateway.service" /etc/systemd/system/rlark-station-gateway.service &&
    systemctl is-enabled rlark-station-gateway >/dev/null 2>&1 && systemctl is-active rlark-station-gateway >/dev/null 2>&1; then return 0; fi
  cp "$B/rlark-station-gateway.service" /etc/systemd/system/rlark-station-gateway.service
  systemctl daemon-reload
  systemctl enable rlark-station-gateway >/dev/null 2>&1
  systemctl restart rlark-station-gateway || fail "the tunnel service did not start: journalctl -u rlark-station-gateway"
  # Its first pass also writes the names and the name servers, which the next steps read.
  for try in $(seq 100); do [ -e /etc/rlark/station/resolv.conf ] && break; sleep 0.2; done
  changed=tunnel
}

# kernel: the modules and settings a node's pod network needs, now and at
# every boot, whatever container runtime the node has.
kernel() {
  for module in overlay br_netfilter; do modprobe $module || fail "the kernel has no $module module"; done
  printf '%s\n' overlay br_netfilter wireguard > /etc/modules-load.d/rlark.conf
  printf '%s\n' 'net.ipv4.ip_forward = 1' 'net.bridge.bridge-nf-call-iptables = 1' > /etc/sysctl.d/90-rlark.conf
  sysctl -q -p /etc/sysctl.d/90-rlark.conf
}

# own_runtime: a containerd of its own for the kubelet; the machine's docker
# and its containerd are not touched, nor restarted.
own_runtime() {
  SOCKET=/run/containerd-fleet/containerd.sock
  [ ! -S $SOCKET ] || ! systemctl is-enabled containerd-fleet >/dev/null 2>&1 || return 0
  install -d /etc/containerd-fleet
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
  systemctl daemon-reload
  systemctl enable --now containerd-fleet >/dev/null 2>&1 || fail "containerd-fleet did not start: journalctl -u containerd-fleet"
  changed=runtime
}

# import_images DIR: the archives a node needs before it can pull anything.
import_images() {
  CTR=$(ctr_of "$SOCKET")
  [ -x "$CTR" ] || fail "cannot find ctr beside the containerd of $SOCKET"
  for archive in "$1"/*.tar; do
    "$CTR" -a "$SOCKET" -n k8s.io images import "$archive" >/dev/null || fail "import $archive"
  done
}

# kubelet_unit: the kubelet of the bundle as a service that reads drop-ins
# from /etc/kubernetes/kubelet.conf.d. Swap stays on for the machine's own
# software; pods get none.
kubelet_unit() {
  install -d /etc/kubernetes/kubelet.conf.d /etc/systemd/system/kubelet.service.d
  cat > /etc/kubernetes/kubelet.conf.d/10-rlark.conf <<CONF
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
failSwapOn: false
memorySwap:
  swapBehavior: NoSwap
CONF
  # Pods get the name servers the tunnel service keeps: the site's where
  # its network declares them (SiteNetwork, sites, dns), else the machine's
  # own. A robot's computer has none of its own to give.
  dns=/etc/kubernetes/kubelet.conf.d/15-rlark-dns.conf
  [ -e $dns ] || { printf 'apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nresolvConf: /etc/rlark/station/resolv.conf\n' > $dns; changed=kubelet; }
  cat > "$B/kubelet.service" <<UNIT
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
  cat > "$B/zz-rlark.conf" <<UNIT
[Service]
Environment="KUBELET_KUBECONFIG_ARGS=--bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf"
Environment="KUBELET_CONFIG_ARGS=--config=/var/lib/kubelet/config.yaml --config-dir=/etc/kubernetes/kubelet.conf.d"
EnvironmentFile=-/var/lib/kubelet/kubeadm-flags.env
ExecStart=
ExecStart=/opt/rlark/bin/kubelet \$KUBELET_KUBECONFIG_ARGS \$KUBELET_CONFIG_ARGS \$KUBELET_KUBEADM_ARGS
UNIT
  if ! cmp -s "$B/kubelet.service" /etc/systemd/system/kubelet.service || ! cmp -s "$B/zz-rlark.conf" /etc/systemd/system/kubelet.service.d/zz-rlark.conf; then
    cp "$B/kubelet.service" /etc/systemd/system/kubelet.service
    cp "$B/zz-rlark.conf" /etc/systemd/system/kubelet.service.d/zz-rlark.conf
    systemctl daemon-reload
    changed=kubelet
  fi
  systemctl enable kubelet >/dev/null 2>&1
}

# node_install SETTINGS STATE: the node's pair of annotations. The spec is
# what the node was installed from: the bundle's tag and a digest of the site
# settings the installer was given (the same on every node of a kind, so
# nothing of one node is among them); the state is "ok" or a short reason.
# Written with the kubelet's own credentials. The digest is kept on the node
# for the upgrade tool, which changes the bundle and not the settings.
node_install() {
  install -d /etc/rlark
  printf '%s' "$1" | sha256sum | cut -c1-12 > /etc/rlark/node-install
  kubectl --kubeconfig /etc/kubernetes/kubelet.conf annotate node "$NODE_NAME" --overwrite \
    "rlark.io/node-install-spec=$(cat "$B/tag").$(cat /etc/rlark/node-install)" "rlark.io/node-install-state=$2" >/dev/null
}
