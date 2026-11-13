#!/usr/bin/env bash
# Issues the cloud registry's server certificate and stores it as Secret rlark-registry/rlark-registry-cloud-tls.
# The registry is one service for the whole cloud site: it listens on its node's VPC address, which cloud nodes
# reach directly and every other site reaches through the Station tunnel. Nodes trust the CA through the
# registry-trust Addon, so nothing here is per pulling node.
# usage: ksyun-registry-cert.sh <ca.crt> <ca.key> <kubeconfig of the cloud cluster> <registry address> [more SAN addresses]
set -euo pipefail
ca_crt=$1 ca_key=$2 kubeconfig=$3; shift 3
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
san=$(printf 'IP:%s,' "$@")
openssl req -newkey rsa:2048 -nodes -keyout "$work/tls.key" -subj '/CN=rlark-registry-ksyun' -out "$work/tls.csr"
printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\nkeyUsage=digitalSignature,keyEncipherment\n' "${san%,}" > "$work/ext"
openssl x509 -req -in "$work/tls.csr" -CA "$ca_crt" -CAkey "$ca_key" -CAserial "$work/ca.srl" -CAcreateserial -days 365 -sha256 -extfile "$work/ext" -out "$work/tls.crt"
openssl verify -CAfile "$ca_crt" "$work/tls.crt"
kubectl --kubeconfig "$kubeconfig" -n rlark-registry create secret tls rlark-registry-cloud-tls --cert="$work/tls.crt" --key="$work/tls.key" --dry-run=client -o yaml |
    kubectl --kubeconfig "$kubeconfig" apply -f -
kubectl --kubeconfig "$kubeconfig" -n rlark-registry rollout restart deployment/rlark-registry-cloud
