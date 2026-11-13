#!/bin/bash
# Isolated run of "this machine becomes the control plane of a cluster"
# (bootstrap/control-plane.sh) in the shape of the site's first use of it: a
# robot cluster whose control plane is on a robot gets a new one on a
# workstation that is a worker of the office cluster; the robots move over;
# later the control plane moves on to a server, from a snapshot.
#
# The machines are containers with systemd and a containerd (a node image
# with Kubernetes taken out), never more than three at a time:
#   office   control plane of the office cluster (made by the mode itself)
#   ws       the workstation: a worker of the office cluster whose tunnel a pod
#            runs, with a process and a container outside Kubernetes that must
#            never restart, and a kubelet setting of its own
#   robot1   control plane of the old robot cluster, built by hand: binaries in
#            /opt/robot-kubernetes, the machine's own containerd, no endpoint name
#   robot2   worker of the old robot cluster
#   server   the machine the control plane moves to at the end
# There is no management plane here. hack/e2e/stationstub answers the
# installer's redeem call, and this script does what station-controller and
# the agents do for the tunnel: it writes each machine's WireGuard document
# from the keys it has seen. The tunnels are real WireGuard interfaces in the
# machines' network namespaces. migrate-node.sh needs the management plane and
# is not run: its Job is, with install.sh's migrate path on the node.
#
#   control-plane.sh bundle             the installer of VERSION with the control plane's images and Calico
#   control-plane.sh up                 stub, office (control plane), ws (its worker, tunnel by a pod, see workstation)
#   control-plane.sh robot_cp ws        run the mode on ws for the robot cluster ("robot_cp ws undo" takes it back)
#   control-plane.sh old                robot1 + robot2: the old robot cluster
#   control-plane.sh move NODE          a robot from the old cluster to the one of ws (BREAK=yes: a join that fails)
#   control-plane.sh reinstall robot1   out of the old cluster, wiped, installed as a worker
#   control-plane.sh server             the control plane from ws to a new machine, from a snapshot
#   control-plane.sh down
# Functions are run by name; cp-rehearsal/NOTES.md has the order they were run in.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../../.." && pwd)
D=${CPR_DIR:-$REPO/../cp-rehearsal/run}
VERSION=${VERSION:-v1.33.13}
KUBE_DIR=${KUBE_DIR:-$REPO/../kube}
CALICO=${CALICO:-$REPO/../calico/v3.30.7/calico.yaml}
CALICO_VERSION=v3.30.7
NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.31.0}
INSTALLER=rlark/node-bootstrap:${TAG:-cpr-$VERSION}
GATEWAY_IMAGE=${GATEWAY_IMAGE:-station-v2/station-wireguard:e2e-move}
ROBOT=cp.cpr-robot.rlark.internal OFFICE=cp.cpr-office.rlark.internal
declare -A IP=([office]=172.18.0.201 [ws]=172.18.0.205 [robot1]=172.18.0.203 [robot2]=172.18.0.204 [server]=172.18.0.209)
declare -A TUN=([office]=10.254.240.1 [ws]=10.254.240.5 [robot1]=10.254.240.3 [robot2]=10.254.240.4 [server]=10.254.240.9)
now() { date +%s.%N; }
since() { printf '%.1f s' "$(echo "$(now) - $1" | bc)"; }
stamp() { echo "$(date +%T) $*"; }
on() { local node=$1; shift; docker exec -i "cpr-$node" "$@"; }
# k NODE ...: kubectl as the administrator of the cluster whose control plane NODE is.
k() { local node=$1; shift; on "$node" env PATH=/opt/rlark/bin:/opt/robot-kubernetes/bin:/usr/bin:/bin kubectl --kubeconfig /etc/kubernetes/admin.conf "$@"; }
exists() { docker inspect "cpr-$1" >/dev/null 2>&1; }

bundle() {
  local images=$D/images refs
  rm -rf "$images" "$D/manifests"; mkdir -p "$images/control-plane" "$D/manifests" "$D/bin"
  refs=$("$KUBE_DIR/$VERSION/kubeadm" config images list --kubernetes-version "$VERSION")
  docker save --platform linux/amd64 $(echo "$refs" | grep -E '/(kube-proxy|pause):') -o "$images/node.tar"
  docker save --platform linux/amd64 $(echo "$refs" | grep -v -E '/(kube-proxy|pause):') calico/kube-controllers:$CALICO_VERSION -o "$images/control-plane/control-plane.tar"
  docker save --platform linux/amd64 calico/node:$CALICO_VERSION calico/cni:$CALICO_VERSION -o "$images/calico.tar"
  echo "$refs" | grep '/pause:' > "$images/pause.ref"
  cp "$CALICO" "$D/manifests/calico.yaml"
  # What a site adds to every cluster it makes goes in after the CNI; here a mark that it did.
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata: {name: site-manifest-applied, namespace: kube-system}\ndata: {by: "the bundle"}\n' > "$D/manifests/site.yaml"
  KUBE_BIN=$KUBE_DIR/$VERSION CNI_BIN=${CNI_BIN:-/opt/cni/bin} IMAGES=$images MANIFESTS=$D/manifests "$REPO/extensions/station/bootstrap/build.sh" "$INSTALLER" "$D/installer.tar"
  # station-gateway as the bundle has it: the administrator's first tunnel on a machine without one.
  docker rm -f cpr-extract >/dev/null 2>&1 || true
  docker create --name cpr-extract "$INSTALLER" >/dev/null && docker cp cpr-extract:/bundle/bin/station-gateway "$D/bin/" && docker rm cpr-extract >/dev/null
}

stub() {
  mkdir -p "$D/pki" "$D/stub/redeem" "$D/stub/answers" "$D/keys" "$D/storage"
  (cd "$D/pki"
   openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=cpr-ca" -keyout ca.key -out ca.crt 2>/dev/null
   openssl req -newkey rsa:2048 -nodes -subj "/CN=cpr-station-api" -keyout api.key -out api.csr 2>/dev/null
   openssl x509 -req -in api.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 30 -out api.crt -extfile <(echo "subjectAltName=DNS:cpr-station-api") 2>/dev/null
   chmod 644 ./*.key)
  local w; w=$(mktemp -d)
  (cd "$REPO/extensions/station" && GOWORK=off CGO_ENABLED=0 go build -trimpath -o "$w/stationstub" ./hack/e2e/stationstub)
  printf 'FROM scratch\nCOPY stationstub /stationstub\nENTRYPOINT ["/stationstub"]\n' > "$w/Dockerfile"
  docker build -q -t cpr/stationstub "$w" >/dev/null; rm -rf "$w"
  docker rm -f cpr-station-api >/dev/null 2>&1 || true
  docker run -d --name cpr-station-api --network kind --user "$(id -u)" -v "$D/pki:/pki:ro" -v "$D/stub:/data" cpr/stationstub \
    --tls-cert-file=/pki/api.crt --tls-key-file=/pki/api.key --dir=/data >/dev/null
}

# machine NAME: a machine with systemd and a containerd and nothing of
# Kubernetes. Its /etc/hosts is a file of its own, as a machine's is: docker
# writes a container's anew at every start.
machine() {
  local name=$1
  docker rm -f -v "cpr-$name" >/dev/null 2>&1 || true
  printf '127.0.0.1 localhost\n%s %s\n' "${IP[$name]}" "$name" > "$D/hosts-$name"
  docker run -d --name "cpr-$name" --hostname "$name" --network kind --ip "${IP[$name]}" --privileged --cgroupns=private \
    --tmpfs /run --tmpfs /tmp -v /var -v /lib/modules:/lib/modules:ro -v /boot:/boot:ro \
    -v "$D/hosts-$name:/etc/hosts" -v "$D/storage:/mnt/site" "$NODE_IMAGE" >/dev/null
  until on "$name" systemctl is-active containerd >/dev/null 2>&1; do sleep 0.5; done
  on "$name" sh -c 'systemctl disable --now kubelet >/dev/null 2>&1; rm -rf /etc/systemd/system/kubelet.service* /usr/lib/systemd/system/kubelet.service* /etc/kubernetes /var/lib/kubelet /opt/cni/bin/*; rm -f /usr/bin/kubeadm /usr/bin/kubelet /usr/bin/kubectl; systemctl daemon-reload'
}

# document NODE: the machine's tunnel as station-controller would compute it
# from the keys that are known: every other machine is a peer at its address.
document() {
  local node=$1 peer peers=
  for peer in "${!IP[@]}"; do
    [[ $peer != "$node" && -e $D/keys/$peer ]] || continue
    peers+="${peers:+,}{\"publicKey\":\"$(cat "$D/keys/$peer")\",\"endpoint\":\"${IP[$peer]}:51820\",\"allowedIPs\":[\"${TUN[$peer]}/32\"],\"keepaliveSeconds\":25}"
  done
  echo "{\"interface\":\"st-wg0\",\"address\":\"${TUN[$node]}/32\",\"listenPort\":51820,\"mtu\":1380,\"peers\":[$peers],\"routes\":[\"10.254.240.0/24\"]}"
}
# tunnels: every machine gets its document the way its agent would store it:
# beside a host service, or for the pod that runs the tunnel of an office node.
tunnels() {
  local node
  for node in "${!IP[@]}"; do
    exists "$node" && [[ -e $D/keys/$node ]] || continue
    document "$node" > "$D/tunnel-$node.json"
    on "$node" sh -c '[ ! -d /run/station-network ] || cat > /run/station-network/config.json' < "$D/tunnel-$node.json"
  done
  ! exists office || [[ ! -e $D/tunnel-ws.json ]] || ! k office get namespace station-system >/dev/null 2>&1 ||
    k office -n station-system create configmap tunnel-documents --from-literal=ws.json="$(cat "$D/tunnel-ws.json")" --dry-run=client -o yaml | k office apply -f - >/dev/null
}
# first_tunnel NODE: what an administrator gives a machine that is to become a
# control plane and is in no cluster: its key is made and declared, and the
# machine gets a document of its own (admit-member.sh bootstrap does this on a site).
first_tunnel() {
  local node=$1
  on "$node" sh -c 'mkdir -p /opt/rlark/bin /etc/rlark/station && cat > /opt/rlark/bin/station-gateway && chmod 755 /opt/rlark/bin/station-gateway' < "$D/bin/station-gateway"
  on "$node" /opt/rlark/bin/station-gateway --public-key > "$D/keys/$node"
  tunnels
  on "$node" sh -c 'cat > /etc/rlark/station/bootstrap.json' < "$D/tunnel-$node.json"
}

# run_cp NODE [undo] [NAME=VALUE ...]: the mode, as the person's one command
# runs it, except that the installer shares the machine's namespaces by
# container name, the machine being a container without a docker of its own.
run_cp() {
  local node=$1 action= env=(); shift
  [[ ${1:-} != undo ]] || { action=undo; shift; }
  for pair in "$@"; do env+=(-e "$pair"); done
  docker run --rm --name "cpr-run-$node" --privileged --net="container:cpr-$node" --pid="container:cpr-$node" ${DOCKER_ARGS:-} "${env[@]}" "$INSTALLER" control-plane $action
}
robot_cp() { run_cp "$1" "${@:2}" CONTROL_PLANE_NAME=$ROBOT POD_SUBNET=10.242.0.0/16 SERVICE_SUBNET=10.243.0.0/16 ETCD_SNAPSHOT_DIR=/mnt/site/etcd-robot; }

# admit NODE CONTROL_PLANE API_SERVER [API_ADDRESS]: what admit-node.sh, the
# controller and Station API do for a machine: a code, and once the machine
# has shown its key, the tunnel for it and for its peers and a join token.
# Prints the code; the answer is written in the background.
admit() {
  local node=$1 cp=$2 api=$3 address=${4:-} code
  code=$(head -c 512 /dev/urandom | tr -dc 'a-z0-9' | cut -c1-8)
  (
    until [[ -s $D/stub/redeem/$code.key ]]; do sleep 0.5; done
    cp "$D/stub/redeem/$code.key" "$D/keys/$node"
    tunnels
    token=${BAD_TOKEN:-$(on "$cp" env PATH=/opt/rlark/bin:/opt/robot-kubernetes/bin:/usr/bin:/bin kubeadm token create --ttl 30m)}
    hash=sha256:$(on "$cp" cat /etc/kubernetes/pki/ca.crt | openssl x509 -pubkey -noout | openssl pkey -pubin -outform der | sha256sum | cut -d' ' -f1)
    cat > "$D/stub/answers/$code.json.part" <<ANSWER
{"state":"ready","cluster_id":"cpr","node_name":"$node","tunnel_address":"${TUN[$node]}","config":$(cat "$D/tunnel-$node.json"),
 "join":{"api_server":"$api","api_server_address":"$address","token":"$token","ca_cert_hash":"$hash"},
 "labels":"${LABELS:-}","taints":"${TAINTS:-}"}
ANSWER
    mv "$D/stub/answers/$code.json.part" "$D/stub/answers/$code.json"
  ) >>"$D/admit.log" 2>&1 &
  echo "$code"
}
# worker NODE CONTROL_PLANE API_SERVER [API_ADDRESS]: the installer's worker mode on a machine.
worker() {
  local code; code=$(admit "$@")
  docker run --rm --name "cpr-run-$1" --privileged --net="container:cpr-$1" --pid="container:cpr-$1" -v "$D/pki/ca.crt:/ca.crt:ro" \
    -e RLARK_SERVER=https://cpr-station-api:18443 -e RLARK_CODE="$code" "$INSTALLER"
}

# up: the office cluster. Its control plane is made by the mode on a machine
# that has only a tunnel; ws joins it with the worker mode, then its tunnel is
# given to a pod (the gateway container of station-agent.yaml, its document
# from a ConfigMap instead of an agent), which is how the workstation's is run.
up() {
  down; stub
  machine office; first_tunnel office
  run_cp office CONTROL_PLANE_NAME=$OFFICE POD_SUBNET=10.244.0.0/16 SERVICE_SUBNET=10.96.0.0/16 ETCD_SNAPSHOT_DIR=/mnt/site/etcd-office
  machine ws
  worker ws office $OFFICE:6443 "${TUN[office]}"
  on ws sh -c "ctr -n k8s.io -a /run/containerd-fleet/containerd.sock images import -" < <(docker save "$GATEWAY_IMAGE")
  k office create namespace station-system
  tunnels
  k office apply -f - <<DS
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: station-agent-network, namespace: station-system}
spec:
  selector: {matchLabels: {app: station-agent-network}}
  template:
    metadata: {labels: {app: station-agent-network}}
    spec:
      hostNetwork: true
      nodeSelector: {station.rlark.io/network: enabled}
      tolerations: [{operator: Exists}]
      containers:
      - name: wireguard
        image: $GATEWAY_IMAGE
        imagePullPolicy: IfNotPresent
        env:
        - {name: NODE_NAME, valueFrom: {fieldRef: {fieldPath: spec.nodeName}}}
        - {name: STATION_GATEWAY_CONFIG, value: /config/none.json}
        - {name: STATION_GATEWAY_COMPUTED_CONFIG, value: "/documents/\$(NODE_NAME).json"}
        - {name: STATION_GATEWAY_PRUNE_ROUTES, value: "true"}
        securityContext: {runAsUser: 0, capabilities: {drop: [ALL], add: [NET_ADMIN]}}
        volumeMounts:
        - {name: key, mountPath: /var/lib/station/wg}
        - {name: documents, mountPath: /documents}
      volumes:
      - {name: key, hostPath: {path: /var/lib/station/wg, type: DirectoryOrCreate}}
      - {name: documents, configMap: {name: tunnel-documents}}
DS
  on ws systemctl disable --now rlark-station-gateway
  k office label node ws --overwrite station.rlark.io/network=enabled
  k office -n station-system rollout status ds/station-agent-network --timeout=120s
  until on ws test -s /var/lib/station/wg/last-applied.json; do sleep 1; done
  workstation
}

# workstation: what makes ws the workstation: what runs on it outside
# Kubernetes (a process of a systemd unit, and a container of the machine's
# own containerd, where a workstation has docker), a kubelet setting of this
# node only, and pods of the office cluster that must go elsewhere when it leaves.
workstation() {
  on ws systemd-run --unit desktop-process --quiet sleep infinity
  on ws sh -c "ctr -n desktop images import - >/dev/null" < <(docker save --platform linux/amd64 busybox:1.36)
  on ws ctr -n desktop run -d --net-host docker.io/library/busybox:1.36 desktop sleep 1000000
  # A setting of this node only, as a workstation's swap handling is.
  on ws sh -c "sed -i '/^maxPods:/d' /var/lib/kubelet/config.yaml && echo 'maxPods: 77' >> /var/lib/kubelet/config.yaml && systemctl restart kubelet"
  until [[ $(k office get node ws -o jsonpath='{.status.capacity.pods}') == 77 ]]; do sleep 1; done
  # Something of the office cluster that runs on ws and has to go elsewhere when ws leaves.
  k office create deployment guest --image="$(cat "$D/images/pause.ref")" --replicas=2
  k office patch deployment guest --type=merge -p '{"spec":{"template":{"spec":{"tolerations":[{"operator":"Exists"}]}}}}'
  k office rollout status deployment guest --timeout=120s
  k office get nodes -o wide; k office get pods -A -o wide | grep -v kube-system
}

# witness: what must be the same before and after on the workstation.
witness() {
  local node=${1:-ws}
  on "$node" sh -c 'echo "process: pid $(systemctl show desktop-process -p MainPID --value) started $(systemctl show desktop-process -p ActiveEnterTimestamp --value)"
    echo "container: pid $(ctr -n desktop task ls | awk "/desktop/ {print \$2, \$3}") started $(ps -o lstart= -p $(ctr -n desktop task ls | awk "/desktop/ {print \$2}"))"
    echo "machine containerd: pid $(systemctl show containerd -p MainPID --value) started $(systemctl show containerd -p ActiveEnterTimestamp --value)"
    echo "tunnel: $(ip -4 -o addr show dev st-wg0 | awk "{print \$4}") ifindex $(cat /sys/class/net/st-wg0/ifindex) $(cat /var/lib/station/wg/private.key | sha256sum | cut -c1-12)"'
}

# probe FROM ADDRESS PORT SECONDS: one TCP connection a second through the
# tunnel; prints how many did not get an answer (refused is an answer).
probe() {
  on "$1" bash -c "lost=0; for i in \$(seq $4); do timeout 1 bash -c 'echo > /dev/tcp/$2/$3' 2>&1 | grep -q -v refused && lost=\$((lost+1)); sleep 1; done; echo \"tunnel probe $1 -> $2: \$lost of $4 lost\""
}

# old: the old robot cluster, made by hand the way the robot's was: robot1
# with Kubernetes in /opt/robot-kubernetes on the machine's own containerd,
# joined to by address; robot2 a worker with the installer's layout. Both
# with a static pod that stands for the device layer.
calico() { # pod subnet
  sed -e "s#veth_mtu: \"0\"#veth_mtu: \"1360\"#" \
      -e "s#^\( *\)- name: CALICO_IPV4POOL_IPIP#\1- name: IP_AUTODETECTION_METHOD\n\1  value: \"interface=st-wg0\"\n\1- name: CALICO_IPV4POOL_CIDR\n\1  value: \"$1\"\n\1- name: FELIX_IGNORELOOSERPF\n\1  value: \"true\"\n&#" "$CALICO"
}
static_pod() {
  on "$1" sh -c 'cat > /etc/kubernetes/manifests/witness.yaml' <<POD
apiVersion: v1
kind: Pod
metadata: {name: witness, namespace: default}
spec:
  hostNetwork: true
  containers: [{name: witness, image: $(cat "$D/images/pause.ref"), imagePullPolicy: Never}]
POD
}
old() {
  machine robot1; first_tunnel robot1
  on robot1 mkdir -p /opt/robot-kubernetes/bin /opt/cni/bin
  for binary in kubeadm kubelet kubectl; do on robot1 sh -c "cat > /opt/robot-kubernetes/bin/$binary && chmod 755 /opt/robot-kubernetes/bin/$binary" < "$KUBE_DIR/$VERSION/$binary"; done
  docker cp "${CNI_BIN:-/opt/cni/bin}/." cpr-robot1:/opt/cni/bin/
  for archive in node.tar control-plane/control-plane.tar calico.tar; do on robot1 ctr -n k8s.io images import - < "$D/images/$archive" >/dev/null; done
  on robot1 sh -ec '
    cat > /etc/systemd/system/robot-tunnel.service <<UNIT
[Service]
ExecStart=/opt/rlark/bin/station-gateway --config /etc/rlark/station/bootstrap.json --computed-config /run/station-network/config.json --prune-routes
ExecStartPre=/bin/mkdir -p /run/station-network
Restart=always
[Install]
WantedBy=multi-user.target
UNIT
    cat > /etc/systemd/system/kubelet.service <<UNIT
[Unit]
Description=kubelet, set up by hand
[Service]
EnvironmentFile=-/var/lib/kubelet/kubeadm-flags.env
ExecStart=/opt/robot-kubernetes/bin/kubelet --bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf --config=/var/lib/kubelet/config.yaml --fail-swap-on=false \$KUBELET_KUBEADM_ARGS
Restart=always
[Install]
WantedBy=multi-user.target
UNIT
    systemctl daemon-reload; systemctl enable --now robot-tunnel kubelet >/dev/null 2>&1
    until ip -4 addr show dev st-wg0 2>/dev/null | grep -q inet; do sleep 0.5; done
    PATH=/opt/robot-kubernetes/bin:$PATH kubeadm init --kubernetes-version '"$VERSION"' --apiserver-advertise-address '"${TUN[robot1]}"' --node-name robot1 \
      --pod-network-cidr 10.242.0.0/16 --service-cidr 10.243.0.0/16 --cri-socket unix:///run/containerd/containerd.sock \
      --ignore-preflight-errors=Swap,FileExisting-conntrack,FileExisting-socat > /var/log/kubeadm-init.log 2>&1
    sed -i "s/\"\$/ --node-ip='"${TUN[robot1]}"'\"/" /var/lib/kubelet/kubeadm-flags.env; systemctl restart kubelet'
  calico 10.242.0.0/16 | k robot1 apply -f - >/dev/null
  static_pod robot1
  machine robot2
  LABELS=rlark.io/node-category=robot,rlark.io/robot-id=robot2 TAINTS=rlark.io/node-category=robot:NoSchedule worker robot2 robot1 "${TUN[robot1]}:6443"
  static_pod robot2
  k robot1 wait --for=condition=Ready node --all --timeout=180s
  k robot1 create deployment marker --image="$(cat "$D/images/pause.ref")" || true
  k robot1 get nodes -o wide; k robot1 get pods -A -o wide | grep -E 'witness|marker'
}

# move NODE: a robot leaves the old cluster for the one whose control plane
# is ws: the Job of migrate-node.sh, in the old cluster, runs the installer on
# the node. BREAK=yes gives it a join token that is none: the join fails and
# the node must be back in the old cluster by itself.
move() {
  local node=$1 code t0 runtime
  runtime=$(on "$node" sed -n 's/.*--container-runtime-endpoint=unix:\/\/\([^ "]*\).*/\1/p' /var/lib/kubelet/kubeadm-flags.env)
  on "$node" ctr -a "$runtime" -n k8s.io images import - < "$D/installer.tar" >/dev/null
  code=$(LABELS=rlark.io/node-category=robot,rlark.io/robot-id=$node TAINTS=rlark.io/node-category=robot:NoSchedule \
    BAD_TOKEN=${BREAK:+abcdef.0123456789abcdef} admit "$node" ws $ROBOT:6443 "${TUN[ws]}")
  k robot1 -n kube-system delete job "node-migrate-$node" --ignore-not-found >/dev/null
  t0=$(now)
  k robot1 apply -f - >/dev/null <<JOB
apiVersion: batch/v1
kind: Job
metadata: {name: node-migrate-$node, namespace: kube-system}
spec:
  backoffLimit: 0
  template:
    spec:
      nodeName: $node
      hostNetwork: true
      hostPID: true
      restartPolicy: Never
      tolerations: [{operator: Exists}]
      containers:
      - name: migrate
        image: $INSTALLER
        imagePullPolicy: IfNotPresent
        securityContext: {privileged: true}
        env:
        - {name: RLARK_SERVER, value: "https://cpr-station-api:18443"}
        - {name: RLARK_CODE, value: "$code"}
        - {name: RLARK_MIGRATE, value: "node"}
        - name: RLARK_CA
          value: |
$(sed 's/^/            /' "$D/pki/ca.crt")
JOB
  stamp "$node: the move is running on the node"
  until on "$node" sh -c '! systemctl is-active -q rlark-node-migrate && grep -q -E "^Done|old kubelet is started" /var/lib/rlark/bootstrap/install.log' 2>/dev/null; do sleep 2; done
  on "$node" cat /var/lib/rlark/bootstrap/install.log | sed 's/^/    /'
  stamp "$node: the installer ended $(since "$t0") after the Job was made"
}

down() {
  local name
  for name in $(docker ps -a --format '{{.Names}}' | grep '^cpr-'); do docker rm -f -v "$name" >/dev/null; done
  # The machines wrote into the shared directory as root.
  [[ ! -d $D/storage ]] || docker run --rm -v "$D:/d" busybox:1.36 rm -rf /d/storage /d/stub
  rm -rf "$D/keys" "$D/pki" "$D"/hosts-* "$D"/tunnel-*.json "$D/admit.log"
}

"$@"
