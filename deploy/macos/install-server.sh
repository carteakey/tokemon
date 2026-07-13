#!/usr/bin/env bash
set -euo pipefail

label="com.tokemon.server"
server_addr="${TOKEMON_SERVER_ADDR:-0.0.0.0:18080}"
ingest_token="${TOKEMON_INGEST_TOKEN:-}"
database="${TOKEMON_DATABASE:-}"
catalog="${TOKEMON_MODEL_CATALOG:-}"
home="${HOME:?HOME is required}"
binary="$(command -v tokemon 2>/dev/null || true)"
uninstall=0

usage() {
  cat <<'EOF'
Usage: install-server.sh --token TOKEN [options]

Options:
  --token TOKEN      ingest token (stored in a mode-0600 config file)
  --binary PATH      existing tokemon binary (default: tokemon on PATH)
  --addr ADDRESS     listen address (default: 0.0.0.0:18080)
  --database PATH    SQLite database path (default: ~/Library/Application Support/Tokemon/tokemon.db)
  --catalog PATH     model catalog YAML to copy into the managed app directory
  --uninstall        unload and remove the server LaunchAgent and config
EOF
}

die() {
  printf 'tokemon server installer: %s\n' "$1" >&2
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
    --addr)
      (($# >= 2)) || die "--addr requires an address"
      server_addr="$2"
      shift 2
      ;;
    --database)
      (($# >= 2)) || die "--database requires a path"
      database="$2"
      shift 2
      ;;
    --catalog)
      (($# >= 2)) || die "--catalog requires a path"
      catalog="$2"
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

config_path="$home/.config/tokemon/server.env"
plist_path="$home/Library/LaunchAgents/$label.plist"
log_dir="$home/Library/Logs/Tokemon"
binary_dir="$home/.local/bin"
binary_path="$binary_dir/tokemon"
app_dir="$home/Library/Application Support/Tokemon"
managed_catalog="$app_dir/models.yaml"
uid="$(id -u)"

if ((uninstall)); then
  launchctl bootout "gui/$uid/$label" >/dev/null 2>&1 || true
  rm -f "$plist_path" "$config_path"
  printf 'Tokemon server removed for %s\n' "$label"
  exit 0
fi

[[ -n "$ingest_token" ]] || die "--token or TOKEMON_INGEST_TOKEN is required"
[[ -n "$binary" && -x "$binary" ]] || die "tokemon binary not found; pass --binary PATH"
[[ "$server_addr" != *$'\n'* && "$server_addr" != *$'\r'* ]] || die "listen address contains a newline"
[[ "$ingest_token" != *$'\n'* && "$ingest_token" != *$'\r'* ]] || die "token contains a newline"

if [[ -z "$database" ]]; then
  database="$app_dir/tokemon.db"
fi
if [[ -z "$catalog" ]]; then
  catalog="$PWD/catalog/models.yaml"
fi

[[ -f "$catalog" ]] || die "model catalog not found; pass --catalog PATH"
[[ "$database" != *$'\n'* && "$database" != *$'\r'* ]] || die "database path contains a newline"
[[ "$catalog" != *$'\n'* && "$catalog" != *$'\r'* ]] || die "catalog path contains a newline"

mkdir -p "$binary_dir" "$app_dir" "$(dirname "$config_path")" "$(dirname "$plist_path")" "$log_dir" "$(dirname "$database")"
umask 077

cp "$binary" "$binary_path.tmp"
chmod 0755 "$binary_path.tmp"
mv "$binary_path.tmp" "$binary_path"

cp "$catalog" "$managed_catalog.tmp"
chmod 0644 "$managed_catalog.tmp"
mv "$managed_catalog.tmp" "$managed_catalog"

{
  printf 'TOKEMON_SERVER_ADDR=%s\n' "$server_addr"
  printf 'TOKEMON_DATABASE=%s\n' "$database"
  printf 'TOKEMON_MODEL_CATALOG=%s\n' "$managed_catalog"
  printf 'TOKEMON_INGEST_TOKEN=%s\n' "$ingest_token"
} > "$config_path"
chmod 0600 "$config_path"

escaped_binary_path="$(xml_escape "$binary_path")"
escaped_config_path="$(xml_escape "$config_path")"
escaped_app_dir="$(xml_escape "$app_dir")"
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
    <string>serve</string>
    <string>--config</string>
    <string>$escaped_config_path</string>
  </array>
  <key>WorkingDirectory</key>
  <string>$escaped_app_dir</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>$escaped_log_dir/server.log</string>
  <key>StandardErrorPath</key>
  <string>$escaped_log_dir/server.error.log</string>
</dict>
</plist>
EOF
chmod 0600 "$plist_path"

if command -v plutil >/dev/null 2>&1; then
  plutil -lint "$plist_path" >/dev/null
fi

launchctl bootout "gui/$uid/$label" >/dev/null 2>&1 || true
launchctl bootstrap "gui/$uid" "$plist_path"
launchctl kickstart -k "gui/$uid/$label"

printf 'Tokemon server installed and started: %s\n' "$label"
printf 'Config: %s\n' "$config_path"
printf 'Database: %s\n' "$database"
printf 'Logs: %s\n' "$log_dir"
