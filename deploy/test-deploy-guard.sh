#!/usr/bin/env bash
set -euo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/tokemon-deploy-guard.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

database="$tmp/data/tokemon.db"
mkdir -p "$(dirname "$database")"
sqlite3 "$database" <<'SQL'
PRAGMA user_version=4;
CREATE TABLE usage_events(total_tokens INTEGER);
INSERT INTO usage_events(total_tokens) VALUES (7);
SQL

marker="$tmp/backup-invoked"
fake_bin="$tmp/tokemon"
cat > "$fake_bin" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" > "$marker"
EOF
chmod 0755 "$fake_bin"

expect_rejected() {
  local destination="$1"
  rm -f "$marker"
  if TOKEMON_BIN="$fake_bin" "$root/deploy/deploy-guard.sh" preflight \
    --database "$database" --backup-destination "$destination" \
    >/dev/null 2>&1; then
    printf 'unsafe backup destination was accepted: %s\n' "$destination" >&2
    exit 1
  fi
  [[ ! -e "$marker" ]] || {
    printf 'backup command ran for rejected destination: %s\n' "$destination" >&2
    exit 1
  }
}

expect_rejected "$tmp/data"
expect_rejected "$tmp/data/backups"
expect_rejected "$tmp/data/../data/snapshots"
expect_rejected "$database"
ln -s "$tmp/data" "$tmp/data-link"
expect_rejected "$tmp/data-link"

mkdir -p "$tmp/off-host"
safe_destination="$tmp/off-host/../off-host/snapshots"
TOKEMON_BIN="$fake_bin" "$root/deploy/deploy-guard.sh" preflight \
  --database "$database" --backup-destination "$safe_destination" >/dev/null
[[ -s "$marker" ]] || {
  printf 'safe backup destination did not invoke backup command\n' >&2
  exit 1
}
grep -F -- '--destination' "$marker" >/dev/null || {
  printf 'backup command did not receive destination\n' >&2
  exit 1
}

printf 'deploy guard smoke: same, nested, alias, symlink, and safe destinations verified\n'
