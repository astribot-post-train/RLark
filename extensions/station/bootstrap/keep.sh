#!/bin/sh
# Keeps a node as the installer made it. A timer of the node runs it after
# every boot and every ten minutes (lib.sh, keep_service); nothing outside
# the machine is needed for it, so a robot that boots without its site comes
# back the same. What the installer wrote and set up is looked at again with
# the installer's own functions, which leave alone what is as written:
#   the kernel modules and settings of the pod network
#   the tunnel service, its unit and that it runs
#   the container runtime's unit, enabled and running
#   the kubelet's unit and drop-ins, enabled and running
# What was not so is put back and said on the Node:
#   rlark.io/node-install-state     ok, or what could not be put back
#   rlark.io/node-install-restored  when something was last put back, and what
# rlark.io/node-install-spec, what the node was installed from, is not its to change.
set -eu
. /var/lib/rlark/bootstrap/lib.sh
. /etc/rlark/node-keep.env
export PATH="/opt/rlark/bin:$PATH"
PENDING=/var/lib/rlark/node-keep.restored
restored=
note() { restored="${restored:+$restored, }$1"; }
report() { # state
  [ -z "$restored" ] || echo "$(date -u +%Y-%m-%dT%H:%M:%SZ): $restored" > "$PENDING"
  # After a boot the API may not answer yet, on a robot away from its site
  # not at all: what was put back is said by the next run that reaches it.
  set -- "rlark.io/node-install-state=$1"
  [ ! -e "$PENDING" ] || set -- "$@" "rlark.io/node-install-restored=$(cat "$PENDING")"
  ! kubectl --kubeconfig /etc/kubernetes/kubelet.conf --request-timeout=10s annotate node "$NODE_NAME" --overwrite "$@" >/dev/null 2>&1 || rm -f "$PENDING"
}
fail() { echo "$(date +%T) FAILED: $*" >&2; report "not as installed: $*"; exit 1; }

before=$(cat /etc/modules-load.d/rlark.conf /etc/sysctl.d/90-rlark.conf /proc/sys/net/ipv4/ip_forward /proc/sys/net/bridge/bridge-nf-call-iptables 2>&1 | sha256sum)
kernel || fail "the kernel modules and settings of the pod network"
[ "$before" = "$(cat /etc/modules-load.d/rlark.conf /etc/sysctl.d/90-rlark.conf /proc/sys/net/ipv4/ip_forward /proc/sys/net/bridge/bridge-nf-call-iptables 2>&1 | sha256sum)" ] || note "the kernel modules and settings of the pod network"

changed=
tunnel_service
[ "$changed" != tunnel ] || note "the tunnel service"

if ! systemctl is-enabled -q "$RUNTIME_UNIT" 2>/dev/null || ! systemctl is-active -q "$RUNTIME_UNIT"; then
  systemctl enable --now "$RUNTIME_UNIT" >/dev/null 2>&1 || fail "the container runtime $RUNTIME_UNIT does not start: journalctl -u $RUNTIME_UNIT"
  note "the container runtime $RUNTIME_UNIT"
fi

changed=
kubelet_unit
if [ "$changed" = kubelet ]; then
  systemctl restart kubelet || fail "the kubelet does not start: journalctl -u kubelet"
  note "the kubelet's unit and drop-ins"
elif ! systemctl is-active -q kubelet; then
  systemctl start kubelet || fail "the kubelet does not start: journalctl -u kubelet"
  note "the kubelet, which was not running"
fi
[ -z "$restored" ] || stamp "put back: $restored"
report ok
