#!/usr/bin/env bash
set -euo pipefail

# Install a Tokemon agent as a user-level service on macOS or Linux. The
# default path consumes a local binary; --version downloads a checksum-verified
# release archive so enrolled machines do not need Go, Docker, or a checkout.

repository="${TOKEMON_RELEASE_REPOSITORY:-carteakey/tokemon}"
release_token="${TOKEMON_RELEASE_TOKEN:-${GITHUB_TOKEN:-}}"
version="${TOKEMON_VERSION:-}"
server_url="${TOKEMON_SERVER_URL:-}"
ingest_token="${TOKEMON_INGEST_TOKEN:-}"
machine_id="${TOKEMON_MACHINE_ID:-}"
interval="${TOKEMON_SCAN_INTERVAL:-1m}"
adapters="${TOKEMON_ADAPTERS:-}"
jsonl_paths="${TOKEMON_JSONL_PATHS:-}"
home="${TOKEMON_HOME:-${HOME:?HOME is required}}"
state_path="${TOKEMON_STATE:-$home/.local/share/tokemon/state.db}"
install_dir="${TOKEMON_INSTALL_DIR:-$home/.local/bin}"
binary=""
no_supervisor=0
uninstall=0
state_explicit=0
install_dir_explicit=0

usage() {
  cat <<'EOF'
Usage: install-agent.sh --server URL --token TOKEN [options]

Install a user-level Tokemon agent on macOS or Linux.

Options:
  --server URL          Tokemon server base URL
  --token TOKEN         ingest token (stored in a mode-0600 config file)
  --binary PATH         existing Tokemon binary
  --version VERSION     download vVERSION from the GitHub release repository
  --repo OWNER/REPO     release repository (default: carteakey/tokemon)
  --release-token TOKEN GitHub token for private release downloads
  --machine-id ID       stable machine ID (default: host name)
  --interval DUR        polling interval (default: 1m)
  --adapters IDS        comma-separated built-in adapter IDs
  --jsonl-paths PATHS   comma-separated generic JSONL paths or globs
  --home PATH            home directory to scan (default: current HOME)
  --state PATH           local cursor database (default: ~/.local/share/tokemon/state.db)
  --install-dir PATH    binary directory (default: ~/.local/bin)
  --no-supervisor       install the binary/config without starting launchd/systemd
  --uninstall            remove service/config but preserve binary and state
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

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
    return
  fi
  die "sha256sum or shasum is required to verify release downloads"
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
    --version)
      (($# >= 2)) || die "--version requires a version"
      version="$2"
      shift 2
      ;;
    --repo)
      (($# >= 2)) || die "--repo requires OWNER/REPO"
      repository="$2"
      shift 2
      ;;
    --release-token)
      (($# >= 2)) || die "--release-token requires a value"
      release_token="$2"
      shift 2
      ;;
    --machine-id)
      (($# >= 2)) || die "--machine-id requires an ID"
      machine_id="$2"
      shift 2
      ;;
    --interval)
      (($# >= 2)) || die "--interval requires a duration"
      interval="$2"
      shift 2
      ;;
    --adapters)
      (($# >= 2)) || die "--adapters requires a comma-separated list"
      adapters="$2"
      shift 2
      ;;
    --jsonl-paths)
      (($# >= 2)) || die "--jsonl-paths requires a comma-separated list"
      jsonl_paths="$2"
      shift 2
      ;;
    --home)
      (($# >= 2)) || die "--home requires a path"
      home="$2"
      shift 2
      ;;
    --state)
      (($# >= 2)) || die "--state requires a path"
      state_path="$2"
      state_explicit=1
      shift 2
      ;;
    --install-dir)
      (($# >= 2)) || die "--install-dir requires a path"
      install_dir="$2"
      install_dir_explicit=1
      shift 2
      ;;
    --no-supervisor)
      no_supervisor=1
      shift
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

if (( !state_explicit )) && [[ -z "${TOKEMON_STATE:-}" ]]; then
  state_path="$home/.local/share/tokemon/state.db"
fi
if (( !install_dir_explicit )) && [[ -z "${TOKEMON_INSTALL_DIR:-}" ]]; then
  install_dir="$home/.local/bin"
fi

case "$(uname -s)" in
  Darwin) platform="darwin" ;;
  Linux) platform="linux" ;;
  *) die "unsupported operating system; use macOS or Linux" ;;
esac
case "$(uname -m)" in
  arm64|aarch64) architecture="arm64" ;;
  x86_64|amd64) architecture="amd64" ;;
  *) die "unsupported CPU architecture: $(uname -m)" ;;
esac

config_path="$home/.config/tokemon/agent.env"
binary_path="$install_dir/tokemon"

if ((uninstall)); then
  if [[ "$platform" == "darwin" ]]; then
    label="com.tokemon.agent"
    uid="$(id -u)"
    launchctl bootout "gui/$uid/$label" >/dev/null 2>&1 || true
    rm -f "$home/Library/LaunchAgents/$label.plist" "$config_path"
  else
    unit_path="$home/.config/systemd/user/tokemon-agent.service"
    systemctl --user disable --now tokemon-agent.service >/dev/null 2>&1 || true
    rm -f "$unit_path" "$config_path"
    systemctl --user daemon-reload >/dev/null 2>&1 || true
  fi
  printf 'Tokemon agent service/config removed; local state preserved at %s\n' "$state_path"
  exit 0
fi

[[ -n "$server_url" ]] || die "--server or TOKEMON_SERVER_URL is required"
[[ -n "$ingest_token" ]] || die "--token or TOKEMON_INGEST_TOKEN is required"
[[ "$server_url" != *$'\n'* && "$server_url" != *$'\r'* ]] || die "server URL contains a newline"
[[ "$ingest_token" != *$'\n'* && "$ingest_token" != *$'\r'* ]] || die "token contains a newline"
[[ "$ingest_token" != *[[:space:]]* ]] || die "token contains whitespace"
[[ "$machine_id" != *$'\n'* && "$machine_id" != *$'\r'* ]] || die "machine ID contains a newline"
[[ "$home" != *$'\n'* && "$home" != *$'\r'* ]] || die "home path contains a newline"
[[ "$state_path" != *$'\n'* && "$state_path" != *$'\r'* ]] || die "state path contains a newline"
[[ "$install_dir" != *$'\n'* && "$install_dir" != *$'\r'* ]] || die "install path contains a newline"

if [[ -z "$machine_id" ]]; then
  if [[ "$platform" == "darwin" ]]; then
    machine_id="$(scutil --get LocalHostName 2>/dev/null || hostname -s)"
  else
    machine_id="$(hostname -s 2>/dev/null || hostname)"
  fi
fi

temporary_dir=""
cleanup() {
  if [[ -n "$temporary_dir" ]]; then
    rm -rf "$temporary_dir"
  fi
}
trap cleanup EXIT

if [[ -z "$binary" && -z "$version" ]]; then
  binary="$(command -v tokemon 2>/dev/null || true)"
fi
if [[ -z "$binary" ]]; then
  [[ -n "$version" ]] || die "pass --binary PATH or --version VERSION"
  version="${version#v}"
  [[ "$version" =~ ^[0-9A-Za-z._-]+$ ]] || die "invalid version: $version"
  command -v curl >/dev/null 2>&1 || die "curl is required to download releases"
  command -v tar >/dev/null 2>&1 || die "tar is required to unpack releases"
  temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/tokemon-agent.XXXXXX")"
  archive_name="tokemon_${version}_${platform}_${architecture}.tar.gz"
  release_base="https://github.com/${repository}/releases/download/v${version}"
  if [[ -n "$release_token" ]]; then
    command -v jq >/dev/null 2>&1 || die "jq is required for private release downloads"
    api_base="https://api.github.com/repos/${repository}"
    auth_header="Authorization: Bearer $release_token"
    accept_header="Accept: application/vnd.github+json"
    curl --fail --location --silent --show-error -H "$auth_header" -H "$accept_header" "$api_base/releases/tags/v${version}" -o "$temporary_dir/release.json"
    archive_url="$(jq -r --arg name "$archive_name" '.assets[] | select(.name == $name) | .url' "$temporary_dir/release.json" | head -1)"
    checksum_url="$(jq -r '.assets[] | select(.name == "checksums.txt") | .url' "$temporary_dir/release.json" | head -1)"
    [[ -n "$archive_url" && "$archive_url" != "null" ]] || die "release asset not found: $archive_name"
    [[ -n "$checksum_url" && "$checksum_url" != "null" ]] || die "release asset not found: checksums.txt"
    curl --fail --location --silent --show-error -H "$auth_header" -H 'Accept: application/octet-stream' "$archive_url" -o "$temporary_dir/$archive_name"
    curl --fail --location --silent --show-error -H "$auth_header" -H 'Accept: application/octet-stream' "$checksum_url" -o "$temporary_dir/checksums.txt"
  else
    curl --fail --location --silent --show-error "$release_base/$archive_name" -o "$temporary_dir/$archive_name" || die "release download failed; set TOKEMON_RELEASE_TOKEN for private GitHub releases"
    curl --fail --location --silent --show-error "$release_base/checksums.txt" -o "$temporary_dir/checksums.txt" || die "checksum download failed; set TOKEMON_RELEASE_TOKEN for private GitHub releases"
  fi
  expected="$(awk -v file="$archive_name" '$2 == file || $2 == "*" file {print $1; exit}' "$temporary_dir/checksums.txt")"
  actual="$(sha256_of "$temporary_dir/$archive_name")"
  [[ -n "$expected" && "$expected" == "$actual" ]] || die "release checksum verification failed for $archive_name"
  tar -xzf "$temporary_dir/$archive_name" -C "$temporary_dir"
  binary="$temporary_dir/tokemon"
fi
[[ -x "$binary" ]] || die "tokemon binary is not executable: $binary"

mkdir -p "$install_dir" "$(dirname "$config_path")" "$(dirname "$state_path")"
umask 077
chmod 0700 "$(dirname "$config_path")" "$(dirname "$state_path")"
if [[ ! -e "$state_path" ]]; then
  : > "$state_path"
fi
chmod 0600 "$state_path"
temporary_binary="$(mktemp "$install_dir/.tokemon.XXXXXX")"
cp "$binary" "$temporary_binary"
chmod 0755 "$temporary_binary"
mv -f "$temporary_binary" "$binary_path"

temporary_config="$(mktemp "$(dirname "$config_path")/.agent.env.XXXXXX")"
{
  printf 'TOKEMON_SERVER_URL=%s\n' "$server_url"
  printf 'TOKEMON_INGEST_TOKEN=%s\n' "$ingest_token"
  printf 'TOKEMON_MACHINE_ID=%s\n' "$machine_id"
  printf 'TOKEMON_SCAN_INTERVAL=%s\n' "$interval"
  printf 'TOKEMON_HOME=%s\n' "$home"
  printf 'TOKEMON_STATE=%s\n' "$state_path"
  if [[ -n "$adapters" ]]; then
    printf 'TOKEMON_ADAPTERS=%s\n' "$adapters"
  fi
  if [[ -n "$jsonl_paths" ]]; then
    printf 'TOKEMON_JSONL_PATHS=%s\n' "$jsonl_paths"
  fi
} > "$temporary_config"
chmod 0600 "$temporary_config"
mv -f "$temporary_config" "$config_path"

if ((no_supervisor)); then
  printf 'Tokemon agent installed at %s\n' "$binary_path"
  printf 'Config: %s\n' "$config_path"
  printf 'Start manually with: %s agent --config %s\n' "$binary_path" "$config_path"
  exit 0
fi

if [[ "$platform" == "darwin" ]]; then
  label="com.tokemon.agent"
  plist_path="$home/Library/LaunchAgents/$label.plist"
  log_dir="$home/Library/Logs/Tokemon"
  uid="$(id -u)"
  mkdir -p "$(dirname "$plist_path")" "$log_dir"
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
  if command -v plutil >/dev/null 2>&1; then
    plutil -lint "$plist_path" >/dev/null
  fi
  launchctl bootout "gui/$uid/$label" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/$uid" "$plist_path"
  launchctl kickstart -k "gui/$uid/$label"
  printf 'Tokemon agent installed and started with launchd\n'
  printf 'Config: %s\n' "$config_path"
  printf 'Logs: %s\n' "$log_dir"
else
  unit_path="$home/.config/systemd/user/tokemon-agent.service"
  mkdir -p "$(dirname "$unit_path")"
  cat > "$unit_path" <<EOF
[Unit]
Description=Tokemon usage metadata agent
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
ExecStart="$binary_path" agent --config "$config_path"
Restart=on-failure
RestartSec=5s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
UMask=0077
Environment=HOME="$home"
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=default.target
EOF
  chmod 0644 "$unit_path"
  if command -v systemctl >/dev/null 2>&1 && systemctl --user daemon-reload >/dev/null 2>&1; then
    if systemctl --user enable tokemon-agent.service >/dev/null 2>&1 && systemctl --user restart tokemon-agent.service >/dev/null 2>&1; then
      printf 'Tokemon agent installed and started with systemd\n'
    else
      printf 'Tokemon agent installed; start or restart systemd user service with: systemctl --user enable --now tokemon-agent.service\n'
    fi
  else
    printf 'Tokemon agent installed; start or restart systemd user service with: systemctl --user enable --now tokemon-agent.service\n'
  fi
  printf 'Config: %s\n' "$config_path"
  printf 'Unit: %s\n' "$unit_path"
fi
