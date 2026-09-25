#!/usr/bin/env bash
set -euo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/tokemon-db-guard.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
database="$tmp/tokemon.db"
dd if=/dev/zero of="$database" bs=4096 count=1 2>/dev/null
if "$root/deploy/check-database.sh" "$database" >/dev/null 2>&1; then
  printf 'zeroed SQLite file was accepted\n' >&2
  exit 1
fi
printf 'database guard smoke: zeroed file refused\n'
