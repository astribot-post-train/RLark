#!/bin/bash
# The migration of the live site, rehearsed: the five nodes start on the images
# that run live on 2026-11-12 and on hand-written configurations of the same
# shape as the live ones; then the rollout's steps are applied one by one while
# pings run between the nodes, and the longest gap of each step is printed.
#
#   migrate.sh start             old images + hand-written ConfigMaps, tunnels up
#   migrate.sh step2 CLUSTER     new images; nodes enroll; gateways still read the ConfigMap
#   migrate.sh compare           hand-written against computed, per node
#   migrate.sh step3 CLUSTER     gateways read the computed configuration
#   migrate.sh step4 CLUSTER     gateways prune dropped routes
#   migrate.sh back CLUSTER      rollback of steps 3 and 4
#   migrate.sh measure LABEL COMMAND...   run COMMAND under the pings and report the gaps
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
source "$HERE/e2e.sh"

LIVE_AGENT=${LIVE_AGENT:-station-v2/station-agent:live-20261112}
LIVE_WIREGUARD=${LIVE_WIREGUARD:-station-v2/station-wireguard:live-20261112}
NODES=(e2e-office/e2e-office-control-plane e2e-office/e2e-office-worker e2e-lab/e2e-lab-control-plane e2e-ksyun/e2e-ksyun-control-plane e2e-ksyun/e2e-ksyun-worker)
# The paths a Run uses: robot <-> both cloud nodes, robot <-> office, cloud -> robot.
PINGS=("e2e-lab-control-plane 198.19.2.57" "e2e-lab-control-plane 198.19.2.196" "e2e-lab-control-plane 198.18.99.1" "e2e-ksyun-control-plane 198.18.99.3" "e2e-office-worker 198.18.99.3")

# handwritten turns the computed documents into what an administrator wrote on
# 2026-11-12: the same peers, endpoints and prefixes, without the additions the
# computation makes (TestComputeCoversLiveSite lists them).
handwritten() {
  local entry cluster node
  for entry in "${NODES[@]}"; do
    cluster=${entry%/*} node=${entry#*/}
    mgmt -n "$cluster" get netenroll "$node" -o jsonpath='{.status.config}' > "$D/computed-$node.json"
  done
  python3 - "$D" <<'PY'
import json, sys
d = sys.argv[1]
drop = {  # node: prefixes the hand-written file did not have
    "e2e-office-control-plane": {"198.19.2.57/32", "198.19.2.196/32"},
    "e2e-ksyun-control-plane": {"198.19.5.12/32"},
    "e2e-office-worker": {"172.27.0.0/24", "172.27.1.0/24"},
}
keys = {n: None for n in ["e2e-office-control-plane", "e2e-office-worker", "e2e-lab-control-plane", "e2e-ksyun-control-plane", "e2e-ksyun-worker"]}
docs = {n: json.load(open(f"{d}/computed-{n}.json")) for n in keys}
for node, doc in docs.items():
    for peer in doc["peers"]:
        peer["allowedIPs"] = [p for p in peer["allowedIPs"] if p not in drop.get(node, ())]
        # The office hub did not dial the 3090; the 3090 dialled it.
        if node == "e2e-office-control-plane" and peer["allowedIPs"] == ["198.18.99.5/32"]:
            peer.pop("endpoint", None), peer.pop("keepaliveSeconds", None)
    doc["routes"] = [p for p in doc["routes"] if p not in drop.get(node, ())]
    json.dump(doc, open(f"{d}/handwritten-{node}.json", "w"))
PY
}

start() {
  local entry cluster node
  for cluster in e2e-office e2e-lab e2e-ksyun; do member "$cluster" -n station-system delete ds station-agent-network station-agent --wait >/dev/null 2>&1 || true; done
  for entry in "${NODES[@]}"; do docker exec "${entry#*/}" sh -c 'ip link del st-wg0 2>/dev/null; ip link del st-hub0 2>/dev/null; rm -f /var/lib/station/wg/last-applied.json'; done
  mgmt delete netenroll -A --all >/dev/null 2>&1 || true
  mgmt -n station-system delete sitenet default >/dev/null 2>&1 || true
  site_network >/dev/null
  # Enroll once with the new images only to learn the nodes' keys, as an
  # administrator read them from the logs; then go back to before the rollout.
  for cluster in e2e-office e2e-lab e2e-ksyun; do agents "$cluster"; enable "$cluster"; done
  until [[ $(mgmt get netenroll -A -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' | grep -c Enrolled) == 5 ]]; do sleep 3; done
  handwritten
  for cluster in e2e-office e2e-lab e2e-ksyun; do member "$cluster" -n station-system delete ds station-agent-network station-agent --wait >/dev/null; done
  mgmt delete netenroll -A --all >/dev/null
  mgmt -n station-system delete sitenet default >/dev/null
  for entry in "${NODES[@]}"; do
    cluster=${entry%/*} node=${entry#*/}
    docker exec "$node" sh -c 'ip link del st-wg0 2>/dev/null; ip link del st-hub0 2>/dev/null; rm -f /var/lib/station/wg/last-applied.json'
  done
  for cluster in e2e-office e2e-lab e2e-ksyun; do
    local arguments=()
    for entry in "${NODES[@]}"; do
      [[ ${entry%/*} == "$cluster" ]] && arguments+=(--from-file="${entry#*/}.json=$D/handwritten-${entry#*/}.json")
    done
    member "$cluster" -n station-system create configmap station-wireguard-config "${arguments[@]}" --dry-run=client -o yaml | member "$cluster" apply -f - >/dev/null
    kind load docker-image --name "$cluster" "$LIVE_AGENT" "$LIVE_WIREGUARD" >/dev/null
    AGENT_IMAGE=$LIVE_AGENT WIREGUARD_IMAGE=$LIVE_WIREGUARD AGENT_MANIFEST=$HERE/station-agent.live-20261112.yaml agents "$cluster"
    member "$cluster" -n station-system rollout status ds/station-agent-network --timeout 180s >/dev/null
  done
  echo "start: live images on hand-written configurations"
}

# measure LABEL COMMAND...: pings every 0.2 s for the duration of COMMAND plus 30 s on each side.
# All pings from one node run in one container so that they have different
# ICMP identifiers; with the same one each would count the others' replies.
measure() {
  local label=$1 i source sources=() commands; shift
  declare -A commands
  for i in "${!PINGS[@]}"; do
    source=${PINGS[$i]% *}
    [[ -n ${commands[$source]:-} ]] || sources+=("$source")
    commands[$source]+="ping -i 0.2 -c 100000 -W 1 ${PINGS[$i]#* } > /out/ping-$i.log 2>&1 & "
  done
  for source in "${sources[@]}"; do
    docker rm -f "e2e-ping-$source" >/dev/null 2>&1 || true
    docker run -d --rm --name "e2e-ping-$source" --network "container:$source" -v "$D:/out" busybox:1.36 sh -c "${commands[$source]} wait" >/dev/null
  done
  sleep 30
  "$@"
  sleep 30
  for source in "${sources[@]}"; do docker exec "e2e-ping-$source" killall -INT ping; done
  for source in "${sources[@]}"; do docker wait "e2e-ping-$source" >/dev/null 2>&1 || true; done
  for i in "${!PINGS[@]}"; do
    python3 -c '
import re, sys
text = open(sys.argv[3]).read()
seqs = [int(m) for m in re.findall(r"seq=(\d+)", text)]
summary = re.search(r"(\d+) packets transmitted", text)
sent = int(summary.group(1)) if summary else max(seqs, default=-1) + 1
longest, previous = 0, -1
for seq in seqs + [sent]:
    longest, previous = max(longest, seq - previous - 1), seq
print(f"{sys.argv[1]:<28} {sys.argv[2]:<38} sent {sent:5d} lost {sent - len(seqs):4d} longest gap {longest * 0.2:5.1f} s")
' "$label" "${PINGS[$i]}" "$D/ping-$i.log"
  done
}

rollout() { member "$1" -n station-system rollout status ds/station-agent-network --timeout 300s >/dev/null; }

# The rollout document's step 2, verbatim.
step2() {
  load "$1"
  member "$1" -n station-system patch ds station-agent-network --type=json -p "$(cat <<JSON
[
 {"op":"replace","path":"/spec/template/spec/containers/0/image","value":"$AGENT_IMAGE"},
 {"op":"replace","path":"/spec/template/spec/containers/1/image","value":"$WIREGUARD_IMAGE"},
 {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--network-identity-file=/var/run/station-observation/identity.json"},
 {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--network-config-file=/var/run/station-network/config.json"},
 {"op":"add","path":"/spec/template/spec/volumes/-","value":{"name":"wireguard-computed","emptyDir":{}}},
 {"op":"add","path":"/spec/template/spec/containers/0/volumeMounts/-","value":{"name":"wireguard-computed","mountPath":"/var/run/station-network"}},
 {"op":"add","path":"/spec/template/spec/containers/1/volumeMounts/-","value":{"name":"wireguard-computed","mountPath":"/var/run/station-network","readOnly":true}},
 {"op":"add","path":"/spec/template/spec/containers/1/env/-","value":{"name":"STATION_GATEWAY_IDENTITY_FILE","value":"/var/run/station-observation/identity.json"}}
]
JSON
)" >/dev/null
  rollout "$1"
}

compare() {
  local entry cluster node
  for entry in "${NODES[@]}"; do
    cluster=${entry%/*} node=${entry#*/}
    mgmt -n "$cluster" get netenroll "$node" -o jsonpath='{.status.config}' > "$D/now-$node.json"
    printf '%-26s %s\n' "$node" "$(python3 "$REPO/extensions/station/deploy/compare-config.py" "$D/handwritten-$node.json" "$D/now-$node.json" | tail -1)"
  done
}

step3() {
  member "$1" -n station-system set env ds/station-agent-network -c wireguard STATION_GATEWAY_COMPUTED_CONFIG=/var/run/station-network/config.json >/dev/null
  rollout "$1"
}

step4() {
  member "$1" -n station-system set env ds/station-agent-network -c wireguard STATION_GATEWAY_PRUNE_ROUTES=true >/dev/null
  rollout "$1"
}

back() {
  member "$1" -n station-system set env ds/station-agent-network -c wireguard STATION_GATEWAY_COMPUTED_CONFIG- STATION_GATEWAY_PRUNE_ROUTES=true >/dev/null
  rollout "$1"
}

restart() { member "$1" -n station-system rollout restart ds/station-agent-network >/dev/null; rollout "$1"; }

"$@"
