#!/usr/bin/env bash
set -euo pipefail

# Run this script from cron, launchd, or a systemd timer. The destination must
# be a mounted off-host path (for example, an encrypted backup volume or an
# rsync-mounted host). The Tokemon backup command is WAL-safe and may run while
# the dashboard is serving; restore is intentionally a separate, offline step.

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
database=${TOKEMON_DATABASE:-$root/data/tokemon.db}
destination=${TOKEMON_BACKUP_DESTINATION:-}
retention=${TOKEMON_BACKUP_RETENTION:-10}

if [[ -z "$destination" ]]; then
  echo "tokemon backup: TOKEMON_BACKUP_DESTINATION is required" >&2
  exit 2
fi

if [[ -n "${TOKEMON_BIN:-}" ]]; then
  command=("$TOKEMON_BIN")
elif command -v tokemon >/dev/null 2>&1; then
  command=("$(command -v tokemon)")
elif [[ -x "$root/tokemon" ]]; then
  command=("$root/tokemon")
else
  command=(go run ./cmd/tokemon)
fi

cd "$root"
"${command[@]}" backup create \
  --database "$database" \
  --destination "$destination" \
  --retention "$retention"
