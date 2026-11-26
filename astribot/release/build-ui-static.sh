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
VITE_IAM_PORTAL=1 npm run build -- --outDir "$1"
