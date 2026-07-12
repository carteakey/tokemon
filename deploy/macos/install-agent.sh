#!/usr/bin/env bash
set -euo pipefail

label="com.tokemon.agent"
server_url="${TOKEMON_SERVER_URL:-}"
ingest_token="${TOKEMON_INGEST_TOKEN:-}"
machine_id="${TOKEMON_MACHINE_ID:-}"
interval="${TOKEMON_SCAN_INTERVAL:-1m}"
home="${HOME:?HOME is required}"
binary="$(command -v tokemon 2>/dev/null || true)"
uninstall=0

usage() {
  cat <<'EOF'
Usage: install-agent.sh --server URL --token TOKEN [options]

Options:
  --server URL       Tokemon server base URL
  --token TOKEN      ingest token (stored in a mode-0600 config file)
  --binary PATH      existing tokemon binary (default: tokemon on PATH)
  --machine-id ID   stable machine ID (default: macOS LocalHostName)
  --interval DUR     polling interval (default: 1m)
  --home PATH        agent home to scan (default: current HOME)
  --uninstall        unload and remove the LaunchAgent and config
EOF
}

die() {
  printf 'tokemon agent installer: %s\n' "$1" >&2
  exit 1
}

xml_escape() {
  local value="$1"
  value="${value//&/&amp;}"
  value="${value//</&lt;}"
  value="${value//>/&gt;}"
  value="${value//\"/&quot;}"
  value="${value//\'/&apos;}"
  printf '%s' "$value"
}

while (($#)); do
  case "$1" in
    --server)
      (($# >= 2)) || die "--server requires a URL"
      server_url="$2"
      shift 2
      ;;
    --token)
      (($# >= 2)) || die "--token requires a value"
      ingest_token="$2"
      shift 2
      ;;
    --binary)
      (($# >= 2)) || die "--binary requires a path"
      binary="$2"
      shift 2
      ;;
    --machine-id)
      (($# >= 2)) || die "--machine-id requires a value"
      machine_id="$2"
      shift 2
      ;;
    --interval)
      (($# >= 2)) || die "--interval requires a duration"
      interval="$2"
      shift 2
      ;;
    --home)
      (($# >= 2)) || die "--home requires a path"
      home="$2"
      shift 2
      ;;
    --uninstall)
      uninstall=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

config_path="$home/.config/tokemon/agent.env"
plist_path="$home/Library/LaunchAgents/$label.plist"
log_dir="$home/Library/Logs/Tokemon"
binary_dir="$home/.local/bin"
binary_path="$binary_dir/tokemon"
uid="$(id -u)"

if ((uninstall)); then
  launchctl bootout "gui/$uid/$label" >/dev/null 2>&1 || true
  rm -f "$plist_path" "$config_path"
  printf 'Tokemon agent removed for %s\n' "$label"
  exit 0
fi

[[ -n "$server_url" ]] || die "--server or TOKEMON_SERVER_URL is required"
[[ -n "$ingest_token" ]] || die "--token or TOKEMON_INGEST_TOKEN is required"
[[ -n "$binary" && -x "$binary" ]] || die "tokemon binary not found; pass --binary PATH"
[[ "$server_url" != *$'\n'* && "$server_url" != *$'\r'* ]] || die "server URL contains a newline"
[[ "$ingest_token" != *$'\n'* && "$ingest_token" != *$'\r'* ]] || die "token contains a newline"
[[ "$machine_id" != *$'\n'* && "$machine_id" != *$'\r'* ]] || die "machine ID contains a newline"

if [[ -z "$machine_id" ]]; then
  machine_id="$(scutil --get LocalHostName 2>/dev/null || hostname -s)"
fi

mkdir -p "$binary_dir" "$(dirname "$config_path")" "$(dirname "$plist_path")" "$log_dir"
umask 077
cp "$binary" "$binary_path.tmp"
chmod 0755 "$binary_path.tmp"
mv "$binary_path.tmp" "$binary_path"

{
  printf 'TOKEMON_SERVER_URL=%s\n' "$server_url"
  printf 'TOKEMON_INGEST_TOKEN=%s\n' "$ingest_token"
  printf 'TOKEMON_MACHINE_ID=%s\n' "$machine_id"
  printf 'TOKEMON_SCAN_INTERVAL=%s\n' "$interval"
  printf 'TOKEMON_HOME=%s\n' "$home"
} > "$config_path"
chmod 0600 "$config_path"

escaped_binary_path="$(xml_escape "$binary_path")"
escaped_config_path="$(xml_escape "$config_path")"
escaped_log_dir="$(xml_escape "$log_dir")"
cat > "$plist_path" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$label</string>
  <key>ProgramArguments</key>
  <array>
    <string>$escaped_binary_path</string>
    <string>agent</string>
    <string>--config</string>
    <string>$escaped_config_path</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>$escaped_log_dir/agent.log</string>
  <key>StandardErrorPath</key>
  <string>$escaped_log_dir/agent.error.log</string>
</dict>
</plist>
EOF
chmod 0600 "$plist_path"

launchctl bootout "gui/$uid/$label" >/dev/null 2>&1 || true
launchctl bootstrap "gui/$uid" "$plist_path"
launchctl kickstart -k "gui/$uid/$label"

printf 'Tokemon agent installed and started: %s\n' "$label"
printf 'Config: %s\n' "$config_path"
printf 'Logs: %s\n' "$log_dir"
