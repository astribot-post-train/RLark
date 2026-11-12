#!/usr/bin/env python3
"""Compare a node's hand-written WireGuard configuration with the computed one.

    compare-config.py LIVE.json COMPUTED.json

LIVE is the node's entry of the station-wireguard-config ConfigMap; COMPUTED is
.status.config of its NetworkEnrollment. Prints what the computed document adds
and what it would remove, and exits 1 when it would remove anything: a node is
switched to the computed document only when this prints no removal.
"""
import json
import sys

live, computed = (json.load(open(path)) for path in sys.argv[1:3])
removed, added = [], []
for field in ("interface", "address", "listenPort", "mtu"):
    if live.get(field) != computed.get(field):
        removed.append(f"{field}: {live.get(field)} -> {computed.get(field)}")
peers = {peer["publicKey"]: peer for peer in computed.get("peers", [])}
for peer in live.get("peers", []):
    key = peer["publicKey"]
    other = peers.pop(key, None)
    if other is None:
        removed.append(f"peer {key}")
        continue
    for field in ("endpoint", "keepaliveSeconds"):
        if peer.get(field) != other.get(field):
            (removed if peer.get(field) else added).append(f"peer {key} {field}: {peer.get(field)} -> {other.get(field)}")
    removed += [f"peer {key} allowedIP {prefix}" for prefix in peer["allowedIPs"] if prefix not in other["allowedIPs"]]
    added += [f"peer {key} allowedIP {prefix}" for prefix in other["allowedIPs"] if prefix not in peer["allowedIPs"]]
added += [f"peer {key}" for key in peers]
removed += [f"route {route}" for route in live.get("routes", []) if route not in computed.get("routes", [])]
added += [f"route {route}" for route in computed.get("routes", []) if route not in live.get("routes", [])]
uplinks = [json.dumps(route, sort_keys=True) for route in computed.get("uplinkRoutes", [])]
removed += [f"uplink {route['destination']}" for route in live.get("uplinkRoutes", []) if json.dumps(route, sort_keys=True) not in uplinks]
for line in added:
    print("+", line)
for line in removed:
    print("-", line)
print(f"{len(added)} added, {len(removed)} removed")
sys.exit(1 if removed else 0)
