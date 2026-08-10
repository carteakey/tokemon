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

preflight verifies the header/integrity/schema/counts and then requires a
Tokemon binary with `backup create` to produce a verified off-host snapshot.
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

"$root/deploy/check-database.sh" "$database"

if [[ "$mode" == preflight ]]; then
  [[ -n "$backup_destination" ]] || {
    printf 'deploy guard: preflight requires --backup-destination (off-host)\n' >&2
    exit 2
  }
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
