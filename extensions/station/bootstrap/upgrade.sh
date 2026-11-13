#!/bin/sh
# Upgrades this node's Kubernetes by one minor version; station-bootstrap runs
# it as a host process from a Job that upgrade-cluster.sh pins to the node.
#   PHASE=certs     on a control-plane node, before anything else: the API
#                   server's certificate and etcd's two must name every
#                   address the node is reached at now. kubeadm reaches etcd
#                   at the address etcd advertises and verifies etcd's
#                   certificate for it, so on a node whose address has changed
#                   since the cluster was made "kubeadm upgrade apply" fails
#                   with etcd already replaced. Certificates that lack a name
#                   are issued anew by kubeadm from the cluster's CA, and etcd
#                   and the API server started again (about a quarter of a minute
#                   without the API; nothing else on any node restarts).
#                   Nothing is done on a node whose certificates are complete.
#   PHASE=images    import the version's images: on every node before the
#                   first is upgraded, because the control plane's upgrade
#                   rolls kube-proxy and CoreDNS out to all of them at once
#   PHASE=control   import the version's images, then "kubeadm upgrade apply"
#                   on a control-plane node or "kubeadm upgrade node" elsewhere
#   PHASE=kubelet   replace kubelet and kubectl and restart the kubelet. A
#                   watchdog is set on the node first: a systemd timer that
#                   puts the old kubelet and its configuration back
#                   WATCHDOG_SECONDS (600) later, whatever has become of the
#                   upgrade's session, the API or the new kubelet
#   PHASE=confirm   the node runs KUBE_VERSION and the API has seen it Ready:
#                   remove the watchdog. Nothing else keeps the new kubelet.
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
stamp() { printf '%s\n' "$(date +%T) $*"; }
fail() { printf '%s\n' "$(date +%T) FAILED: $*" >&2; exit 1; }
BIN=$(dirname "$(systemctl show kubelet -p ExecStart --value | sed -n 's/.*path=\([^ ;]*kubelet\) .*/\1/p')")
[ -x "$BIN/kubelet" ] || fail "cannot find the kubelet binary of kubelet.service"
SOCKET=$(sed -n 's/.*--container-runtime-endpoint=unix:\/\/\([^ "]*\).*/\1/p' /var/lib/kubelet/kubeadm-flags.env)
[ -S "$SOCKET" ] || fail "cannot find the node's container runtime socket"
# ctr is the one beside the containerd that serves that socket: a node's
# containerd need not be in the PATH, nor be the only one on the node.
CTR=$(dirname "$(readlink "/proc/$(ss -Hxlp | awk -v s="$(readlink -f "$SOCKET")" '$5 == s' | sed -n 's/.*pid=\([0-9]*\).*/\1/p')/exe")")/ctr
[ -x "$CTR" ] || fail "cannot find ctr beside the containerd of $SOCKET"
from=$("$BIN/kubelet" --version | awk '{print $2}')
keep=/var/backups/rlark-kube-upgrade/$from
[ "$PHASE" = rollback ] || install -d "$keep"
export PATH="$BIN:$PATH"
WATCHDOG=rlark-kube-upgrade-watchdog
WATCH=/var/lib/rlark/kube-upgrade
disarm() {
  systemctl disable --now $WATCHDOG.timer >/dev/null 2>&1 || true
  rm -f /etc/systemd/system/$WATCHDOG.timer /etc/systemd/system/$WATCHDOG.service "$WATCH/restore.sh"
  systemctl daemon-reload
}
# A kubelet checks for swap only when it starts. On a node where swap was
# turned on under a running kubelet that is not configured for it, no kubelet
# starts again, the old one included: nothing is changed on such a node.
if [ "$PHASE" != rollback ] && [ "$(wc -l < /proc/swaps)" -gt 1 ] &&
  ! grep -rqs -E '^failSwapOn: *false' /var/lib/kubelet/config.yaml /etc/kubernetes/kubelet.conf.d &&
  ! grep -qs -e '--fail-swap-on=false' /var/lib/kubelet/kubeadm-flags.env /etc/default/kubelet /etc/sysconfig/kubelet; then
  fail "swap is on ($(sed -n 2p /proc/swaps | cut -d' ' -f1)) and this node's kubelet is not configured for it (failSwapOn: false): a restarted kubelet would exit at once. Turn the swap off or declare failSwapOn: false for the node, then upgrade it"
fi

# The node's name: upgrade-cluster.sh names it; run by hand, the kubelet's
# flags or the hostname do.
node=${NODE_NAME:-$(cat /var/lib/kubelet/kubeadm-flags.env /etc/default/kubelet /etc/sysconfig/kubelet 2>/dev/null | sed -n 's/.*--hostname-override=\([^ "]*\).*/\1/p' | head -1)}
[ -n "$node" ] || node=$(hostname | tr '[:upper:]' '[:lower:]')

case $PHASE in
certs)
  M=/etc/kubernetes/manifests P=/etc/kubernetes/pki
  [ -e $M/kube-apiserver.yaml ] || { stamp "not a control-plane node: no certificates to look at"; exit 0; }
  admin() { kubectl --kubeconfig /etc/kubernetes/admin.conf -n kube-system "$@"; }
  etcdctl() { admin exec "etcd-$node" -- etcdctl --endpoints https://127.0.0.1:2379 --cacert $P/etcd/ca.crt --cert $P/etcd/server.crt --key $P/etcd/server.key "$@"; }
  # What the node is reached at: where its API server and its etcd say they
  # are (the manifests are what an address change is made in), and its
  # address in the station's tunnel, which no kubeadm configuration names.
  endpoint=$(sed -n 's/^ *kubeadm.kubernetes.io\/kube-apiserver.advertise-address.endpoint: *//p' $M/kube-apiserver.yaml)
  client=$(sed -n 's/^ *kubeadm.kubernetes.io\/etcd.advertise-client-urls: *//p' $M/etcd.yaml)
  peer=$(sed -n 's/^ *- --initial-advertise-peer-urls=//p' $M/etcd.yaml)
  tunnel=$(ip -4 -o addr show dev st-wg0 2>/dev/null | awk '{print $4}' | cut -d/ -f1)
  install -d "$WATCH"
  admin get configmap kubeadm-config -o jsonpath='{.data.ClusterConfiguration}' > "$WATCH/ClusterConfiguration.before.yaml"
  lacking=$("$B/station-bootstrap" cert-sans "$WATCH/ClusterConfiguration.before.yaml" $P "${endpoint%:*}" $(echo "$client,$peer" | tr , '\n' | sed 's#^https://##; s#:[0-9]*$##') $tunnel 2>&1 > "$WATCH/ClusterConfiguration") || fail "$lacking"
  if [ -n "$lacking" ]; then
    echo "$lacking" | while read -r line; do stamp "$line"; done
    dir=/var/backups/rlark-kube-upgrade/certs-$(date +%Y%m%d%H%M%S)
    install -d -m 700 "$dir" "$dir/replaced"
    (umask 077 && tar -cpf "$dir/etc-kubernetes.tar" -C / etc/kubernetes)
    etcdctl snapshot save /var/lib/etcd/rlark-snapshot-before-certs.db > "$dir/etcd-snapshot.log" 2>&1 || { tail -5 "$dir/etcd-snapshot.log" >&2; fail "etcd snapshot"; }
    mv /var/lib/etcd/rlark-snapshot-before-certs.db "$dir/etcd-snapshot.db"
    cp "$WATCH/ClusterConfiguration.before.yaml" "$dir/"
    # kubeadm would take the node's name and address from the hostname and the
    # default route; they are the ones the node has in the cluster.
    { printf 'apiVersion: %s\nkind: InitConfiguration\nlocalAPIEndpoint: {advertiseAddress: "%s", bindPort: %s}\nnodeRegistration: {name: %s, criSocket: "unix://%s"}\n---\n' \
        "$(sed -n 's/^apiVersion: //p' "$WATCH/ClusterConfiguration")" "${endpoint%:*}" "${endpoint##*:}" "$node" "$SOCKET"; cat "$WATCH/ClusterConfiguration"; } > "$dir/kubeadm.yaml"
    # Only the certificates are taken out of the archive on the way back: the
    # kubelet makes every static pod anew whose manifest is written again.
    pid() { ss -Hltnp "sport = :$1" | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | head -1; }
    undo="tar -xpf $dir/etc-kubernetes.tar -C / etc/kubernetes/pki && kill \$(ss -Hltnp 'sport = :${client##*:}' | sed -n 's/.*pid=\\([0-9]*\\).*/\\1/p' | head -1) \$(ss -Hltnp 'sport = :${endpoint##*:}' | sed -n 's/.*pid=\\([0-9]*\\).*/\\1/p' | head -1)"
    stamp "kept in $dir: /etc/kubernetes (etc-kubernetes.tar), etcd (etcd-snapshot.db), the cluster's kubeadm configuration (ClusterConfiguration.before.yaml)"
    stamp "to put the old certificates back, as root on this node (the kubelet starts etcd and the API server again, the API is away for about a minute; the names stay in the kubeadm configuration): $undo"
    for crt in $(echo "$lacking" | cut -d' ' -f1); do
      # kubeadm keeps a certificate it finds. etcd reads its certificate from
      # the file for every connection: no failure may leave the node without it.
      mv "$P/$crt" "$P/${crt%.crt}.key" "$dir/replaced/"
      kubeadm init phase certs "$(echo "${crt%.crt}" | tr / -)" --config "$dir/kubeadm.yaml" > "$dir/kubeadm-$(basename "$crt").log" 2>&1 ||
        { tar -xpf "$dir/etc-kubernetes.tar" -C / etc/kubernetes/pki; tail -15 "$dir/kubeadm-$(basename "$crt").log" >&2; fail "kubeadm init phase certs for $crt; the old certificates are back"; }
    done
    stamp "issued anew by kubeadm $(kubeadm version -o short): $(echo "$lacking" | cut -d' ' -f1 | tr '\n' ' ')"
    # The names go into the cluster's kubeadm configuration: kubeadm issues a
    # certificate from it whenever it makes one anew.
    admin create configmap kubeadm-config --from-file="ClusterConfiguration=$WATCH/ClusterConfiguration" --dry-run=client -o yaml | admin replace -f - >/dev/null
    # A static pod's container is started again by the kubelet when its
    # process ends; etcd first, the API server does not come up without it.
    t0=$(date +%s)
    old=$(pid "${client##*:}"); kill "$old"
    while kill -0 "$old" 2>/dev/null; do sleep 1; done
    tries=0
    until curl -fsS -m 2 --cacert $P/etcd/ca.crt --cert $P/etcd/healthcheck-client.crt --key $P/etcd/healthcheck-client.key "$client/health" >/dev/null 2>&1; do
      tries=$((tries + 1))
      [ $tries -lt 150 ] || fail "etcd does not answer at $client 150 s after its start with the new certificates. Back: $undo"
      sleep 1
    done
    stamp "etcd answers at $client, the address it advertises, $(( $(date +%s) - t0 )) s after it was stopped"
    t0=$(date +%s)
    # The API server closes its port at once and then waits a minute for
    # its watchers to leave: it is not waited for.
    old=$(pid "${endpoint##*:}"); kill "$old"
    sleep 3
    kill -9 "$old" 2>/dev/null || true
    tries=0
    until curl -fsS -m 2 --cacert $P/ca.crt "https://$endpoint/readyz" >/dev/null 2>&1; do
      tries=$((tries + 1))
      [ $tries -lt 150 ] || fail "the API server is not ready at $endpoint 150 s after its start with the new certificate. Back: $undo"
      sleep 1
    done
    stamp "the API server is ready at $endpoint $(( $(date +%s) - t0 )) s after it was stopped"
  fi
  # A member that etcd knows at another peer address than the one it is
  # started with (the address of the day the cluster was made) is one that a
  # member joining later could not reach.
  member=$(etcdctl member list | grep ", $(sed -n 's/^ *- --name=//p' $M/etcd.yaml), ")
  registered=$(echo "$member" | cut -d, -f4 | tr -d ' ')
  if [ "$registered" != "$peer" ]; then
    etcdctl member update "${member%%,*}" --peer-urls="$peer" >/dev/null
    stamp "etcd member ${member%%,*}: peer address $registered -> $peer"
  fi
  [ -n "$lacking" ] || [ "$registered" != "$peer" ] || stamp "the certificates name every address of this node: nothing to do"
  ;;
images|control)
  for archive in "$B"/images/*.tar; do
    "$CTR" -a "$SOCKET" -n k8s.io images import "$archive" >/dev/null || fail "import $archive"
  done
  stamp "images of $KUBE_VERSION imported"
  [ "$PHASE" = control ] || exit 0
  [ -e "$keep/kubeadm" ] || cp -a "$BIN/kubeadm" "$keep/kubeadm"
  [ -d "$keep/etc-kubernetes" ] || cp -a /etc/kubernetes "$keep/etc-kubernetes"
  [ -d "$keep/kubelet" ] || { install -d "$keep/kubelet" && cp -a /var/lib/kubelet/config.yaml /var/lib/kubelet/kubeadm-flags.env "$keep/kubelet/"; }
  # upgrade-cluster.sh took this snapshot just before it started this phase.
  # A run that goes on after a failed one finds the control plane at
  # KUBE_VERSION already: its snapshot is not of the state before the upgrade,
  # and the one that is stays.
  snapshot=/var/lib/etcd/rlark-snapshot-before-$KUBE_VERSION.db
  if [ -e "$snapshot" ]; then
    if grep -q "kube-apiserver:$KUBE_VERSION\$" /etc/kubernetes/manifests/kube-apiserver.yaml; then rm -f "$snapshot"; else mv "$snapshot" "$keep/etcd-snapshot.db"; fi
  fi
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
    [ -n "$(kubectl --kubeconfig /etc/kubernetes/kubelet.conf get node "$node" -o jsonpath='{.metadata.annotations.kubeadm\.alpha\.kubernetes\.io/cri-socket}')" ] ||
      kubectl --kubeconfig /etc/kubernetes/kubelet.conf annotate node "$node" "kubeadm.alpha.kubernetes.io/cri-socket=unix://$SOCKET" >/dev/null
    kubeadm upgrade node $patches > "$keep/upgrade-node.log" 2>&1 || { tail -25 "$keep/upgrade-node.log" >&2; fail "kubeadm upgrade node"; }
    stamp "node configuration at $KUBE_VERSION"
  fi
  ;;
kubelet)
  [ "$from" != "$KUBE_VERSION" ] || { stamp "the kubelet is already $KUBE_VERSION"; exit 0; }
  [ -e "$keep/bin/kubelet" ] || { install -d "$keep/bin" && cp -a "$BIN/kubelet" "$BIN/kubectl" "$keep/bin/"; }
  install -m 755 "$B/bin/kubelet" "$BIN/kubelet.new" && mv "$BIN/kubelet.new" "$BIN/kubelet"
  install -m 755 "$B/bin/kubectl" "$BIN/kubectl.new" && mv "$BIN/kubectl.new" "$BIN/kubectl"
  # The watchdog: the way back needs neither the kubelet, nor the API, nor
  # this process. It survives a reboot (the timer then counts anew).
  install -d "$WATCH"
  rm -f "$WATCH/restored"
  cat > "$WATCH/restore.sh" <<RESTORE
#!/bin/sh
# Written by upgrade.sh before the kubelet of $KUBE_VERSION was started: back to $from.
journalctl -u kubelet --no-pager --since "-5min" > "$keep/kubelet-failed.log" 2>&1
install -m 755 "$keep/bin/kubelet" "$BIN/kubelet.old" && mv "$BIN/kubelet.old" "$BIN/kubelet"
install -m 755 "$keep/bin/kubectl" "$BIN/kubectl.old" && mv "$BIN/kubectl.old" "$BIN/kubectl"
cp -a "$keep/kubelet/config.yaml" "$keep/kubelet/kubeadm-flags.env" /var/lib/kubelet/
systemctl disable --now $WATCHDOG.timer
systemctl restart kubelet
# Once, and the outcome is left here: nothing tries again.
state="NOT healthy 2 min after its start either (journalctl -u kubelet): the node needs a person"
for try in \$(seq 60); do
  if curl -fsS -m 2 http://127.0.0.1:10248/healthz >/dev/null 2>&1; then state=healthy; break; fi
  sleep 2
done
date "+%F %T the kubelet of $KUBE_VERSION was not confirmed; back on $from, whose kubelet is \$state" > "$WATCH/restored"
[ "\$state" = healthy ]
RESTORE
  printf '[Unit]\nDescription=Back to the kubelet of %s unless the upgrade to %s is confirmed\n[Service]\nType=oneshot\nExecStart=/bin/sh %s/restore.sh\n' "$from" "$KUBE_VERSION" "$WATCH" > /etc/systemd/system/$WATCHDOG.service
  printf '[Timer]\nOnActiveSec=%s\nAccuracySec=1\n[Install]\nWantedBy=timers.target\n' "${WATCHDOG_SECONDS:-600}" > /etc/systemd/system/$WATCHDOG.timer
  systemctl daemon-reload
  systemctl enable $WATCHDOG.timer >/dev/null 2>&1
  systemctl restart $WATCHDOG.timer
  stamp "watchdog set: back on $from in ${WATCHDOG_SECONDS:-600} s unless confirmed"
  systemctl restart kubelet
  tries=0
  until curl -fsS -m 2 http://127.0.0.1:10248/healthz >/dev/null 2>&1; do
    tries=$((tries + 1))
    if [ $tries -ge 60 ]; then
      # No reason to wait for the timer: the same way back, now.
      systemctl start $WATCHDOG.service || true
      grep -v -E 'Failed to ensure lease|Unable to register node|node .* not found' "$keep/kubelet-failed.log" | tail -15 >&2
      fail "the kubelet of $KUBE_VERSION was not healthy 2 min after its start; its log is $keep/kubelet-failed.log. $(cat "$WATCH/restored")"
    fi
    sleep 2
  done
  stamp "kubelet $("$BIN/kubelet" --version | awk '{print $2}') healthy"
  ;;
confirm)
  [ "$from" = "$KUBE_VERSION" ] || fail "the node runs $from, not $KUBE_VERSION: $(cat "$WATCH/restored" 2>/dev/null)"
  disarm
  stamp "watchdog removed: the node stays on $KUBE_VERSION"
  ;;
rollback)
  disarm
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
    "$CTR" -a "$SOCKET" -n k8s.io run --rm --mount "type=bind,src=/var/lib,dst=/var/lib,options=rbind:rw" --mount "type=bind,src=$keep,dst=/keep,options=rbind:ro" \
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
*) fail "PHASE must be certs, images, control, kubelet, confirm or rollback" ;;
esac
