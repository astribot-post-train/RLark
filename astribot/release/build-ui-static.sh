#!/usr/bin/env bash
# Build only the statics used by Astribot's Nginx and IAM deployment.
set -euo pipefail

if [[ $# != 1 || $1 != /* ]]; then
  echo "Usage: $0 /absolute/new-output-directory" >&2
  exit 2
fi
if [[ -e $1 ]]; then
  echo "The output directory already exists: $1" >&2
  exit 2
fi

repository=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
cd "$repository/apps/rlark-ui"
output=$1
VITE_IAM_PORTAL=1 npm run build -- --outDir "$output"

# Match the stable public URLs copied by rlark-ui.Dockerfile.
hero=("$output"/assets/astribot-login-hero-*.jpg)
logo=("$output"/assets/astribot-logo-light-*.png)
if [[ ${#hero[@]} != 1 || ! -f ${hero[0]} || ${#logo[@]} != 1 || ! -f ${logo[0]} ]]; then
  echo "Expected one built IAM hero and one built IAM logo" >&2
  exit 1
fi
cp -- "$repository/astribot/release/login.html" "$output/login.html"
cp -- "${hero[0]}" "$output/login-hero.jpg"
cp -- "${logo[0]}" "$output/astribot-logo-light.png"
