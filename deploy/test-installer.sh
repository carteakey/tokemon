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
  TOKEMON_INGEST_TOKEN=test-token \
  bash "$root/deploy/install-agent.sh" \
    --binary "$tmp/tokemon" --server https://hub.example.ts.net \
    --machine-id installer-test --no-supervisor

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
  TOKEMON_INGEST_TOKEN=test-token \
  bash "$root/deploy/install-agent.sh" \
    --binary "$tmp/tokemon" --server https://hub.example.ts.net \
    --machine-id installer-test --no-supervisor
[[ "$(cat "$state")" == 'updated-cursor' ]] || { printf 'upgrade replaced state\n' >&2; exit 1; }

# Noninteractive stdin remains supported without putting the token in argv.
stdin_home="$tmp/stdin-home"
stdin_install_dir="$tmp/stdin-bin"
stdin_state="$stdin_home/state/state.db"
mkdir -p "$(dirname "$stdin_state")"
printf 'stdin-state\n' > "$stdin_state"
printf '%s\n' 'stdin-token' | env -u TOKEMON_INGEST_TOKEN \
  TOKEMON_HOME="$stdin_home" TOKEMON_INSTALL_DIR="$stdin_install_dir" TOKEMON_STATE="$stdin_state" \
  bash "$root/deploy/install-agent.sh" \
    --binary "$tmp/tokemon" --server https://hub.example.ts.net \
    --token-stdin --machine-id installer-stdin --no-supervisor
grep -F 'TOKEMON_INGEST_TOKEN=stdin-token' "$stdin_home/.config/tokemon/agent.env" >/dev/null || {
  printf 'stdin token was not written to the protected config\n' >&2
  exit 1
}

# Inline token arguments fail closed. Environment-backed installs are sampled
# while slowed mktemp calls keep the installer alive; no argv entry may contain
# the token.
argv_secret='process-argv-regression-token'
wrappers="$tmp/wrappers"
mkdir -p "$wrappers"
real_mktemp="$(command -v mktemp)"
cat > "$wrappers/mktemp" <<EOF
#!/usr/bin/env bash
set -e
"$real_mktemp" "\$@"
sleep 1
EOF
chmod 0755 "$wrappers/mktemp"
ps_args() {
  ps -axo command= 2>/dev/null || ps -eo args= 2>/dev/null || true
}
argv_log="$tmp/argv.log"
PATH="$wrappers:$PATH" TOKEMON_HOME="$home" TOKEMON_INSTALL_DIR="$install_dir" TOKEMON_STATE="$state" \
  TOKEMON_INGEST_TOKEN="$argv_secret" \
  bash "$root/deploy/install-agent.sh" \
    --binary "$tmp/tokemon" --server https://hub.example.ts.net \
    --machine-id installer-argv --no-supervisor >"$argv_log" 2>&1 &
installer_pid=$!
argv_leak=0
while kill -0 "$installer_pid" 2>/dev/null; do
  ps_snapshot="$(ps_args)"
  while IFS= read -r process_line; do
    case "$process_line" in
      *"$argv_secret"*)
        case "$process_line" in
          *"ps -axo command="*|*"ps -eo args="*) continue ;;
        esac
        argv_leak=1
        break
        ;;
    esac
  done <<< "$ps_snapshot"
  ((argv_leak == 0)) || break
  sleep 0.05
done
wait "$installer_pid"
((argv_leak == 0)) || {
  printf 'ingest token appeared in installer process argv\n' >&2
  exit 1
}

# Private release downloads must keep the release token out of curl argv too.
# The fake curl validates that the token is supplied through a mode-0600
# config file, then holds the process open so the parent argv scan can observe
# the installer and curl command lines before the intentional download error.
release_secret='release-process-argv-regression-token'
release_wrappers="$tmp/release-wrappers"
mkdir -p "$release_wrappers"
cat > "$release_wrappers/curl" <<'EOF'
#!/usr/bin/env bash
set -eu
config=''
while (($#)); do
  if [[ "$1" == --config ]]; then
    config="$2"
    shift 2
  else
    shift
  fi
done
[[ -n "$config" && -f "$config" ]] || exit 2
mode="$(stat -c '%a' "$config" 2>/dev/null || stat -f '%Lp' "$config" 2>/dev/null || true)"
[[ "$mode" == 600 ]] || exit 3
grep -F -- "Authorization: Bearer ${TOKEMON_RELEASE_TOKEN:?}" "$config" >/dev/null
sleep 1
exit 1
EOF
chmod 0755 "$release_wrappers/curl"
release_log="$tmp/release-argv.log"
PATH="$release_wrappers:$PATH" TOKEMON_HOME="$home" TOKEMON_INSTALL_DIR="$install_dir" \
  TOKEMON_RELEASE_TOKEN="$release_secret" \
  bash "$root/deploy/install-agent.sh" --version 0.0.1 \
    --server https://hub.example.ts.net --machine-id installer-release-argv --no-supervisor \
    >"$release_log" 2>&1 &
release_pid=$!
release_argv_leak=0
while kill -0 "$release_pid" 2>/dev/null; do
  ps_snapshot="$(ps_args)"
  while IFS= read -r process_line; do
    case "$process_line" in
      *"$release_secret"*)
        case "$process_line" in
          *"ps -axo command="*|*"ps -eo args="*) continue ;;
        esac
        release_argv_leak=1
        break
        ;;
    esac
  done <<< "$ps_snapshot"
  ((release_argv_leak == 0)) || break
  sleep 0.05
done
if wait "$release_pid"; then
  printf 'private release download unexpectedly succeeded\n' >&2
  exit 1
fi
((release_argv_leak == 0)) || {
  printf 'release token appeared in installer or curl process argv\n' >&2
  exit 1
}

if bash "$root/deploy/install-agent.sh" \
  --binary "$tmp/tokemon" --server https://hub.example.ts.net \
  --token "$argv_secret" --machine-id installer-inline --no-supervisor \
  >"$tmp/inline.log" 2>&1; then
  printf 'inline token argument was accepted\n' >&2
  exit 1
fi
grep -F 'inline --token is not accepted' "$tmp/inline.log" >/dev/null || {
  printf 'inline token rejection did not explain the safe alternatives\n' >&2
  exit 1
}
printf 'installer smoke: pass\n'
