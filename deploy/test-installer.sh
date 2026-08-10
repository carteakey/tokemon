#!/usr/bin/env bash
set -euo pipefail

# Linux smoke for the cross-platform installer. macOS runs the same script in
# the release workflow; supervisor startup is intentionally disabled here.

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/tokemon-installer-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
home="$tmp/home"
install_dir="$tmp/bin"
state="$home/state/state.db"
mkdir -p "$home/state"
printf 'preserve-this-cursor\n' > "$state"
CGO_ENABLED=0 go build -trimpath -o "$tmp/tokemon" "$root/cmd/tokemon"

TOKEMON_HOME="$home" TOKEMON_INSTALL_DIR="$install_dir" TOKEMON_STATE="$state" \
  bash "$root/deploy/install-agent.sh" \
    --binary "$tmp/tokemon" --server https://hub.example.ts.net \
    --token test-token --machine-id installer-test --no-supervisor

[[ -x "$install_dir/tokemon" ]] || { printf 'installer did not install binary\n' >&2; exit 1; }
config="$home/.config/tokemon/agent.env"
config_mode="$(stat -c '%a' "$config" 2>/dev/null || stat -f '%Lp' "$config" 2>/dev/null || true)"
[[ "$config_mode" == 600 ]] || {
  printf 'installer config is not mode 0600\n' >&2
  exit 1
}
[[ "$(cat "$state")" == 'preserve-this-cursor' ]] || { printf 'state was not preserved\n' >&2; exit 1; }

# An upgrade replaces only the binary/config and keeps the state database.
printf 'updated-cursor\n' > "$state"
TOKEMON_HOME="$home" TOKEMON_INSTALL_DIR="$install_dir" TOKEMON_STATE="$state" \
  bash "$root/deploy/install-agent.sh" \
    --binary "$tmp/tokemon" --server https://hub.example.ts.net \
    --token test-token --machine-id installer-test --no-supervisor
[[ "$(cat "$state")" == 'updated-cursor' ]] || { printf 'upgrade replaced state\n' >&2; exit 1; }
printf 'installer smoke: pass\n'
