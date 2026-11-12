#!/bin/bash
# Builds the installer image of the node bootstrap and its docker archive.
#
#   build.sh IMAGE_REF OUT.tar
#
# What goes into the bundle is the site's choice, given by the environment:
#   KUBE_BIN   directory with kubeadm, kubelet and kubectl of the cluster's version
#   CNI_BIN    directory with the CNI plugins
#   IMAGES     directory with the image archives a node needs before it can pull
#              (pause, kube-proxy, the CNI's images, station-agent, the RLark node
#              agent) and pause.ref, the reference of the pause image among them
#   CERTS_D    optional containerd certs.d directory: how the node reaches the site's registries
set -euo pipefail
REF=$1 OUT=$2
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
W=$(mktemp -d); trap 'rm -rf "$W"' EXIT
mkdir -p "$W/bundle/bin" "$W/bundle/cni" "$W/bundle/images"
(cd "$REPO/extensions/station-gateway" && GOWORK=off CGO_ENABLED=0 go build -trimpath -o "$W/bundle/bin/station-gateway" .)
(cd "$REPO/extensions/station" && GOWORK=off CGO_ENABLED=0 go build -trimpath -o "$W/station-bootstrap" ./bootstrap)
for binary in kubeadm kubelet kubectl; do install -m 755 "$KUBE_BIN/$binary" "$W/bundle/bin/"; done
[[ -z ${CNI_BIN:-} ]] || find "$CNI_BIN" -maxdepth 1 -type f -perm -u+x -exec install -m 755 {} "$W/bundle/cni/" \;
cp "$IMAGES"/*.tar "$W/bundle/images/"
[[ ! -e $IMAGES/pause.ref ]] || cp "$IMAGES/pause.ref" "$W/bundle/images/"
[[ -z ${CERTS_D:-} ]] || cp -r "$CERTS_D" "$W/bundle/certs.d"
install -m 755 "$HERE/install.sh" "$W/bundle/install.sh"
install -m 755 "$HERE/upgrade.sh" "$W/bundle/upgrade.sh"
cat > "$W/Dockerfile" <<'DOCKERFILE'
FROM scratch
COPY bundle /bundle
COPY station-bootstrap /station-bootstrap
ENTRYPOINT ["/station-bootstrap"]
DOCKERFILE
docker build -q -t "$REF" "$W" >/dev/null
docker save "$REF" -o "$OUT"
chmod 644 "$OUT"
echo "$REF $(du -h "$OUT" | cut -f1) $OUT"
