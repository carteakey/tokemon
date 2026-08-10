#!/usr/bin/env bash
set -euo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/tokemon-server-installer.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
home="$tmp/home"
mkdir -p "$home" "$tmp/bin" "$tmp/wrappers"
printf 'catalog: test\n' > "$tmp/catalog.yaml"

# The server installer is macOS-specific; these no-op launchctl/plutil shims
# let the config/plist path be tested on Linux CI without starting a service.
cat > "$tmp/wrappers/launchctl" <<'EOF'
#!/usr/bin/env bash
sleep 1
exit 0
EOF
cat > "$tmp/wrappers/plutil" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod 0755 "$tmp/wrappers/launchctl" "$tmp/wrappers/plutil"

secret='server-process-argv-regression-token'
server_log="$tmp/server.log"
PATH="$tmp/wrappers:$PATH" HOME="$home" TOKEMON_INGEST_TOKEN="$secret" \
  bash "$root/deploy/macos/install-server.sh" \
    --binary /usr/bin/true --catalog "$tmp/catalog.yaml" --database "$tmp/tokemon.db" \
    >"$server_log" 2>&1 &
server_pid=$!
argv_leak=0
while kill -0 "$server_pid" 2>/dev/null; do
  process_line="$(ps -p "$server_pid" -o command= 2>/dev/null || true)"
  [[ "$process_line" != *"$secret"* ]] || { argv_leak=1; break; }
  sleep 0.05
done
wait "$server_pid"
((argv_leak == 0)) || {
  printf 'server token appeared in installer process argv\n' >&2
  exit 1
}

config="$home/.config/tokemon/server.env"
plist="$home/Library/LaunchAgents/com.tokemon.server.plist"
mode="$(stat -c '%a' "$config" 2>/dev/null || stat -f '%Lp' "$config" 2>/dev/null || true)"
[[ "$mode" == 600 ]] || { printf 'server config is not mode 0600\n' >&2; exit 1; }
grep -F 'TOKEMON_INGEST_TOKEN=server-process-argv-regression-token' "$config" >/dev/null || {
  printf 'server token was not written to the protected config\n' >&2
  exit 1
}
! grep -F "$secret" "$plist" >/dev/null || {
  printf 'server token appeared in LaunchAgent plist\n' >&2
  exit 1
}
grep -F -- '<string>--config</string>' "$plist" >/dev/null || {
  printf 'server LaunchAgent does not use the config path\n' >&2
  exit 1
}

if HOME="$home" TOKEMON_INGEST_TOKEN= \
  bash "$root/deploy/macos/install-server.sh" \
    --binary /usr/bin/true --catalog "$tmp/catalog.yaml" --database "$tmp/tokemon.db" \
    --token "$secret" >/dev/null 2>&1; then
  printf 'server inline token argument was accepted\n' >&2
  exit 1
fi
printf 'server installer smoke: config/plist secret boundary verified\n'
