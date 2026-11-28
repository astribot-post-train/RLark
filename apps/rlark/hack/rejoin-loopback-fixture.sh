#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(git -C "$script_dir" rev-parse --show-toplevel)"
workspace_dir="$(mktemp -d)"
trap 'rm -rf "$workspace_dir"' EXIT

(
  cd "$workspace_dir"
  go work init "$repo_root/apps/rlark" "$repo_root/astribot/station"
)

export GOWORK="$workspace_dir/go.work"
export RLARK_REJOIN_LOOPBACK=1
export RLARK_REJOIN_FIXTURE_MODE="${RLARK_REJOIN_FIXTURE_MODE:-success}"

cd "$repo_root/apps/rlark"
go test -count=1 -tags=rejoin_integration ./pkg/gateway -run '^TestRejoinLoopbackFixture$' -v
