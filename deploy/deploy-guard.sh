#!/usr/bin/env bash
set -euo pipefail

# Compose release gate. `preflight` is fail-closed and must run before any
# recreate; `postflight` validates the new process without mutating data.

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
mode=""
database="${TOKEMON_DATABASE:-$root/data/tokemon.db}"
backup_destination="${TOKEMON_BACKUP_DESTINATION:-}"
health_url="${TOKEMON_HEALTH_URL:-http://127.0.0.1:18787/healthz}"
expected_schema="${TOKEMON_EXPECTED_SCHEMA:-4}"

usage() {
  cat <<'EOF'
Usage: deploy-guard.sh preflight|postflight [options]

  --database PATH       persistent SQLite path
  --backup-destination  off-host destination for the immediate WAL-safe backup (preflight)
  --health URL           health endpoint (postflight)
  --expected-schema N   schema accepted by the release (default: 4)

preflight verifies the header/integrity/schema/counts, rejects destinations
that resolve to the live database directory or any child of it, and then
requires a Tokemon binary with `backup create` to produce a verified off-host
snapshot. The destination must be a separate mounted/off-host path.
postflight repeats the database guard and checks `/healthz`.
EOF
}

while (($#)); do
  case "$1" in
    preflight|postflight) [[ -z "$mode" ]] || { usage >&2; exit 2; }; mode="$1"; shift ;;
    --database) (($# >= 2)) || { usage >&2; exit 2; }; database="$2"; shift 2 ;;
    --backup-destination) (($# >= 2)) || { usage >&2; exit 2; }; backup_destination="$2"; shift 2 ;;
    --health) (($# >= 2)) || { usage >&2; exit 2; }; health_url="$2"; shift 2 ;;
    --expected-schema) (($# >= 2)) || { usage >&2; exit 2; }; expected_schema="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'deploy guard: unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done
[[ -n "$mode" ]] || { usage >&2; exit 2; }
export TOKEMON_EXPECTED_SCHEMA="$expected_schema"

canonical_path() {
  local input="$1" path suffix candidate parent base
  [[ -n "$input" ]] || return 1
  if [[ "$input" == /* ]]; then
    path="$input"
  else
    path="$PWD/$input"
  fi

  # Resolve the nearest existing ancestor so a new destination is checked
  # against the real, symlink-resolved live data directory as well.
  suffix=""
  candidate="$path"
  while [[ ! -e "$candidate" && "$candidate" != "/" ]]; do
    base="${candidate##*/}"
    suffix="/$base$suffix"
    candidate="${candidate%/*}"
    [[ -n "$candidate" ]] || candidate="/"
  done
  [[ -d "$candidate" ]] || return 1
  parent="$(cd -P -- "$candidate" && pwd -P)" || return 1
  printf '%s%s\n' "$parent" "$suffix"
}

canonical_file_path() {
  local input="$1" path parent base
  [[ -n "$input" ]] || return 1
  if [[ "$input" == /* ]]; then
    path="$input"
  else
    path="$PWD/$input"
  fi
  if [[ -e "$path" && ! -d "$path" ]]; then
    parent="${path%/*}"
    base="${path##*/}"
    parent="$(cd -P -- "${parent:-/}" && pwd -P)" || return 1
    printf '%s/%s\n' "$parent" "$base"
  else
    canonical_path "$path"
  fi
}

path_is_same_or_nested() {
  local base="$1" candidate="$2"
  [[ "$candidate" == "$base" || "$candidate" == "$base"/* ]]
}

validate_backup_destination() {
  local live_db live_data destination
  live_db="$(canonical_file_path "$database")" || {
    printf 'deploy guard: cannot canonicalize database path: %s\n' "$database" >&2
    exit 1
  }
  live_data="${live_db%/*}"
  destination="$(canonical_path "$backup_destination")" || {
    printf 'deploy guard: backup destination parent is unavailable: %s\n' "$backup_destination" >&2
    exit 2
  }
  [[ "$destination" != "/" ]] || {
    printf 'deploy guard: backup destination cannot be filesystem root\n' >&2
    exit 2
  }
  if path_is_same_or_nested "$live_data" "$destination" || [[ "$destination" == "$live_db" ]]; then
    printf 'deploy guard: backup destination must be outside live database directory (%s)\n' "$live_data" >&2
    exit 2
  fi
  if [[ -e "$backup_destination" && ! -d "$backup_destination" ]]; then
    printf 'deploy guard: backup destination is not a directory: %s\n' "$backup_destination" >&2
    exit 2
  fi
  TOKEMON_CANONICAL_DATABASE="$live_db"
  TOKEMON_CANONICAL_BACKUP_DESTINATION="$destination"
  export TOKEMON_CANONICAL_DATABASE TOKEMON_CANONICAL_BACKUP_DESTINATION
  database="$live_db"
  backup_destination="$destination"
}

"$root/deploy/check-database.sh" "$database"

if [[ "$mode" == preflight ]]; then
  [[ -n "$backup_destination" ]] || {
    printf 'deploy guard: preflight requires --backup-destination (off-host)\n' >&2
    exit 2
  }
  validate_backup_destination
  backup_bin="${TOKEMON_BIN:-$(command -v tokemon 2>/dev/null || true)}"
  [[ -x "$backup_bin" ]] || {
    printf 'deploy guard: preflight requires TOKEMON_BIN with backup create\n' >&2
    exit 127
  }
  "$backup_bin" backup create --database "$database" --destination "$backup_destination" --retention "${TOKEMON_BACKUP_RETENTION:-10}"
  printf 'deploy guard: preflight passed; Compose recreate may proceed\n'
else
  command -v curl >/dev/null 2>&1 || { printf 'deploy guard: curl is required for postflight\n' >&2; exit 127; }
  curl --fail --silent --show-error "$health_url" >/dev/null || {
    printf 'deploy guard: health check failed\n' >&2
    exit 1
  }
  printf 'deploy guard: postflight passed\n'
fi
