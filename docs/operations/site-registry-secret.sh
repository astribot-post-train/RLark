#!/usr/bin/env bash
# Issues the site registries' server certificate and enters it, with the registry's other credentials, as the
# RLark secret object "site-registry" (the Addon site-registry reads it on every cluster it is distributed to);
# then makes the image registry credential for registry.rlark.internal:5443, distributed to all clusters, with the
# user and password of the existing credential for 10.11.14.128:5443 (copied from one API answer into the next request).
# The same user and password go into the secret as proxy-username / proxy-password: a site cache (role=cache) logs in
# to the origin with them.
# Run it in a real terminal on the machine that holds the CA key: the key is only read by openssl, the new
# certificate key and every other value go from files straight into the request body; nothing is printed or kept.
# Rerun it to renew the certificate or rotate a value; the object is replaced in place.
#
# usage: site-registry-secret.sh <ca.crt> <ca.key> <registry config.yml> <htpasswd>
#   <registry config.yml>  the running registry's distribution config: its storage keys and http secret are reused
#   <htpasswd>             the running registry's htpasswd: existing pull credentials stay valid
# env: GATEWAY (default http://10.11.14.128:19000), CLUSTERS (default rlark-local-dev-gpu,rlark-ksyun-gpu,rlark-robot-s1u-01),
#      SAN (default: the name plus every address the registries answer on today; rerun with the new addresses when a
#      registry moves, image references use the name)
set -euo pipefail
ca_crt=$1 ca_key=$2 config=$3 htpasswd=$4
G=${GATEWAY:-http://10.11.14.128:19000}
san=${SAN:-DNS:registry.rlark.internal,DNS:origin.rlark.internal,IP:10.11.14.128,IP:10.254.240.1,IP:10.0.2.57,IP:10.254.240.2}
[ -t 0 ] || { echo "请在真正的终端里运行（要输入 RLark 管理员口令）"; exit 1; }
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT; chmod 700 "$work"

openssl req -newkey rsa:2048 -nodes -keyout "$work/tls.key" -subj /CN=registry.rlark.internal -out "$work/tls.csr" 2>/dev/null
printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\nkeyUsage=digitalSignature,keyEncipherment\n' "$san" > "$work/ext"
# The certificate cannot outlive its CA.
days=$(( ($(date -d "$(openssl x509 -in "$ca_crt" -noout -enddate | cut -d= -f2)" +%s) - $(date +%s)) / 86400 - 1 ))
openssl x509 -req -in "$work/tls.csr" -CA "$ca_crt" -CAkey "$ca_key" -CAserial "$work/ca.srl" -CAcreateserial \
  -days $(( days < 365 ? days : 365 )) -sha256 -extfile "$work/ext" -out "$work/tls.crt" 2>/dev/null
openssl verify -CAfile "$ca_crt" "$work/tls.crt"
openssl x509 -in "$work/tls.crt" -noout -enddate -ext subjectAltName

read -r -s -p 'RLark 管理员口令（不显示）: ' P; echo
TOKEN=$(jq -n --arg u admin --arg p "$P" '{username:$u,password:$p}' |
  curl -sS -H 'Content-Type: application/json' -d @- "$G/api/v1/auth/login" | jq -r '.token // .data.token // empty'); unset P
[ -n "$TOKEN" ] || { echo "登录失败"; exit 1; }

list=$(curl -sS -H "Authorization: Bearer $TOKEN" "$G/api/v1/image-registries")
source=$(jq -r 'first(.[] | select(.registry == "registry.rlark.internal:5443")) // first(.[] | select(.registry == "10.11.14.128:5443")) | .id' <<<"$list")
user=$(jq -r --arg id "$source" '.[] | select(.id == $id) | .username' <<<"$list")
curl -sS -H "Authorization: Bearer $TOKEN" "$G/api/v1/image-registries/$source/password" | jq -r .password | tr -d '\n' > "$work/password"
if jq -e 'any(.[]; .registry == "registry.rlark.internal:5443")' <<<"$list" >/dev/null; then
  echo "image registry credential for registry.rlark.internal:5443 already exists"
else
  jq -n --arg user "$user" --rawfile password "$work/password" \
    '{name:"site-registry", registry:"registry.rlark.internal:5443", username:$user, password:$password,
      clusterSelection:{mode:"All", clusters:[]}}' > "$work/registry.json"
  curl -sS -o "$work/reply" -w 'image registry credential: HTTP %{http_code}\n' -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' -d @"$work/registry.json" "$G/api/v1/image-registries"
  jq -c '{id, name, registry, username, clusterSelection}' "$work/reply" 2>/dev/null || head -c 300 "$work/reply"
fi

python3 -c 'import json,sys,yaml; c=yaml.safe_load(open(sys.argv[1])); s=c["storage"]["s3"]
json.dump({"s3-accesskey": s["accesskey"], "s3-secretkey": s["secretkey"], "http-secret": c["http"]["secret"]}, sys.stdout)' \
  "$config" > "$work/values.json"
jq -n --slurpfile v "$work/values.json" --rawfile htpasswd "$htpasswd" --rawfile crt "$work/tls.crt" --rawfile key "$work/tls.key" \
  --arg user "$user" --rawfile password "$work/password" \
  --arg clusters "${CLUSTERS:-rlark-local-dev-gpu,rlark-ksyun-gpu,rlark-robot-s1u-01}" \
  '{name:"site-registry", description:"站点镜像仓库（Addon site-registry）的存储密钥、htpasswd、TLS、缓存回源账号",
    clusters:($clusters | split(",")),
    data:($v[0] + {htpasswd:$htpasswd, "tls.crt":$crt, "tls.key":$key, "proxy-username":$user, "proxy-password":$password})}' > "$work/body.json"
method=POST url=$G/api/v1/secrets
curl -sS -H "Authorization: Bearer $TOKEN" "$url" | jq -e '(.data // .) | any(.name == "site-registry")' >/dev/null &&
  method=PUT url=$url/site-registry
curl -sS -o "$work/reply" -w "$method HTTP %{http_code}\n" -X "$method" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d @"$work/body.json" "$url"
jq -c '{success, message, error}' "$work/reply" 2>/dev/null || head -c 300 "$work/reply"
curl -sS -H "Authorization: Bearer $TOKEN" "$G/api/v1/secrets" | jq -c '(.data // .)[] | select(.name == "site-registry") | {name, clusters, keys}'

echo "site-registry: done"
