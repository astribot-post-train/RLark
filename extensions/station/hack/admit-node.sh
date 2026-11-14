#!/usr/bin/env bash
# Admit a machine as a node of a registered cluster and print the one line to
# run on it. The Gateway gives the code out (POST /api/v1/station/node-admissions),
# exactly as the console's "Add node" does: it records who and when, keeps
# only the code's hash, and makes the line. For a machine of a site whose
# leaves stand behind a device of their own (the site's leafUplink.setup),
# the line first makes that device let the machine out (it asks for the
# device's password), then installs.
#
#   admit-node.sh CLUSTER_ID NODE_NAME API_SERVER [label=value ...]
#
#   API_SERVER   host:port of the cluster's API as the machine reaches it without the tunnel
# Environment: RLARK_URL (the Gateway's origin), RLARK_TOKEN_FILE (a file
# holding an administrator service account's token; it is read, never
# printed), RLARK_CA (the CA of the Gateway's certificate, for https), TAINTS
# ("k=v:Effect,..."), SITE (the site the machine stands in, when it is not its
# cluster's), ADDRESS (pin the tunnel address), UPLINK (the gateway of a
# machine whose default route does not lead out, where its site declares no
# leafUplink), TTL (default "2 hours").
set -euo pipefail
: "${RLARK_URL:?the Gateway origin, e.g. http://127.0.0.1:9000}" "${RLARK_TOKEN_FILE:?a file with an administrator service account token}"
CLUSTER=$1 NODE=$2 API_SERVER=$3; shift 3
ttl=$(( $(date -d "+${TTL:-2 hours}" +%s) - $(date +%s) ))
request=$(python3 - "$CLUSTER" "$NODE" "$API_SERVER" "$ttl" "$@" <<'PY'
import json, os, sys
cluster, node, api_server, ttl, *labels = sys.argv[1:]
body = {"cluster_id": cluster, "node_name": node, "api_server": api_server, "ttl_seconds": int(ttl)}
for field, variable in (("site", "SITE"), ("address", "ADDRESS"), ("uplink", "UPLINK")):
    if os.environ.get(variable):
        body[field] = os.environ[variable]
if os.environ.get("TAINTS"):
    body["taints"] = os.environ["TAINTS"].split(",")
if labels:
    body["labels"] = dict(label.split("=", 1) for label in labels)
print(json.dumps(body))
PY
)
header=$(umask 077 && mktemp)
trap 'rm -f "$header"' EXIT
printf 'Authorization: Bearer %s\n' "$(<"$RLARK_TOKEN_FILE")" > "$header"
answer=$(curl -sS ${RLARK_CA:+--cacert "$RLARK_CA"} -H @"$header" -H 'Content-Type: application/json' \
  -w '\n%{http_code}' -X POST --data "$request" "${RLARK_URL%/}/api/v1/station/node-admissions")
status=${answer##*$'\n'} answer=${answer%$'\n'*}
[[ $status == 200 || $status == 201 ]] || { echo "the Gateway answered $status: $answer; nothing was admitted" >&2; exit 1; }
python3 -c '
import json, sys
answer = json.load(sys.stdin)
print("admitted %(node_name)s into %(cluster_id)s, code valid until %(expires_at)s" % answer["admission"], file=sys.stderr)
print(answer["command"])' <<<"$answer"
