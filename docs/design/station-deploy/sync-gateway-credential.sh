#!/usr/bin/env bash
set -euo pipefail

# The directory is mounted into Gateway; replacing a file inside it is visible
# to the running container, unlike replacing a separately bind-mounted file.
directory=${STATION_GATEWAY_CREDENTIAL_DIR:?set STATION_GATEWAY_CREDENTIAL_DIR to the Gateway credential directory}
install -d -m 700 -- "$directory"
temporary=$(mktemp "$directory/.token.XXXXXXXX")
trap 'rm -f -- "$temporary"' EXIT
chmod 600 "$temporary"
kubectl -n "${STATION_NAMESPACE:-station-system}" get secret "${STATION_GATEWAY_SECRET:-station-gateway-summary-credential}" -o jsonpath='{.data.token}' | base64 -d > "$temporary"
if [ "$(wc -c < "$temporary")" -lt 32 ]; then
  echo 'Station gateway credential is missing or too short' >&2
  exit 1
fi
mv -f -- "$temporary" "$directory/token"
echo 'Station gateway credential synchronized; verify the node-network endpoint.'
