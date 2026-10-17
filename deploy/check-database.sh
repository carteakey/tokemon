#!/usr/bin/env bash
set -euo pipefail

# Read-only database guard for deployment automation. It intentionally emits
# only structural metadata (schema and aggregate counts), never row values or
# tokens/secrets.

database="${1:-}"
expected_schema="${TOKEMON_EXPECTED_SCHEMA:-4}"
[[ -n "$database" ]] || { printf 'database guard: usage: check-database.sh DATABASE\n' >&2; exit 2; }
[[ -f "$database" ]] || { printf 'database guard: database is missing: %s\n' "$database" >&2; exit 1; }
[[ ! -L "$database" ]] || { printf 'database guard: symlink database is refused\n' >&2; exit 1; }

header="$(dd if="$database" bs=16 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n')"
[[ "$header" == 53514c69746520666f726d6174203300 ]] || {
  printf 'database guard: SQLite header check failed\n' >&2
  exit 1
}
command -v sqlite3 >/dev/null 2>&1 || {
  printf 'database guard: sqlite3 is required for read-only integrity checks\n' >&2
  exit 127
}

# URI mode=ro prevents this check from creating WAL/SHM files or changing the
# journal mode. The path is controlled by the deployment operator, not row
# data, and is quoted for SQLite string syntax.
uri="file:${database}?mode=ro"
integrity="$(sqlite3 "$uri" 'PRAGMA integrity_check;' 2>/dev/null)" || {
  printf 'database guard: SQLite could not open the database read-only\n' >&2
  exit 1
}
[[ "$integrity" == ok ]] || {
  printf 'database guard: integrity_check=%s\n' "$integrity" >&2
  exit 1
}
schema="$(sqlite3 "$uri" 'PRAGMA user_version;' 2>/dev/null)" || exit 1
[[ "$schema" == "$expected_schema" ]] || {
  printf 'database guard: unsupported schema version %s (expected %s)\n' "$schema" "$expected_schema" >&2
  exit 1
}
stats="$(sqlite3 "$uri" "SELECT COUNT(*), COALESCE(SUM(COALESCE(total_tokens,0)),0) FROM usage_events;" 2>/dev/null)" || {
  printf 'database guard: usage totals query failed\n' >&2
  exit 1
}
events="${stats%%|*}"
tokens="${stats#*|}"
printf 'database guard: schema=%s events=%s lifetime_tokens=%s integrity=ok\n' "$schema" "$events" "$tokens"
