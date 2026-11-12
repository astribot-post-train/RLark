#!/bin/bash
# The four Station images for the isolated run, from this tree with the local
# Go toolchain (no base image is pulled): build-local.sh TAG
set -euo pipefail
TAG=$1
REPO=$(cd "$(dirname "$0")/../../../.." && pwd)
W=$(mktemp -d); trap 'rm -rf "$W"' EXIT
image() { # name entrypoint user directory package
  mkdir -p "$W/$1$(dirname "$2")" "$W/$1/tmp" && chmod 1777 "$W/$1/tmp"
  (cd "$REPO/$4" && GOWORK=off CGO_ENABLED=0 go build -trimpath -o "$W/$1$2" "$5")
  printf 'FROM scratch\nCOPY . /\nUSER %s\n' "$3" > "$W/$1.Dockerfile"
  # station-services holds several programs in the real image and is started by path.
  [[ $1 == station-services ]] || printf 'ENTRYPOINT ["%s"]\n' "$2" >> "$W/$1.Dockerfile"
  docker build -q -f "$W/$1.Dockerfile" -t "station-v2/$1:$TAG" "$W/$1" >/dev/null
}
image station-services /usr/local/bin/station-api 65532 apps/rlark ./cmd/station-api
image station-controller /station-controller 65532 extensions/station ./cmd/station-controller
image station-agent /usr/local/bin/station-agent 65532 apps/rlark ./cmd/station-agent
image station-wireguard /station-gateway 0 extensions/station-gateway .
docker images --format '{{.Repository}}:{{.Tag}} {{.Size}}' | grep "^station-v2/.*:$TAG"
