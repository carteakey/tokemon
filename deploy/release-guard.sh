#!/usr/bin/env bash
set -euo pipefail

# Fail closed when a release or Docker context contains persistent SQLite
# state, WAL sidecars, backup snapshots, incidents, or private fixtures. The
# check is intentionally path based: it does not read file contents or print
# potentially sensitive values.

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
mode="tracked"
context=""

usage() {
  cat <<'EOF'
Usage: release-guard.sh [--tracked] [--context DIR]

  --tracked       inspect Git tracked files (default)
  --context DIR   inspect a release/Docker context without reading contents
EOF
}

while (($#)); do
  case "$1" in
    --tracked) mode="tracked"; shift ;;
    --context)
      (($# >= 2)) || { usage >&2; exit 2; }
      mode="context"
      context="$2"
      shift 2
      ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'release guard: unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done

case "$mode" in
  tracked)
    paths=()
    while IFS= read -r path; do paths+=("$path"); done < <(git -C "$root" ls-files)
    ;;
  context)
    [[ -d "$context" ]] || { printf 'release guard: context is not a directory\n' >&2; exit 2; }
    paths=()
    while IFS= read -r path; do paths+=("$path"); done < <(find -L "$context" -path "$context/.git" -prune -o -type f -print | sed "s#^$context/##")
    ;;
esac

for path in "${paths[@]}"; do
  normalized="/${path#./}"
  case "$normalized" in
    */data/*|*/backups/*|*/incidents/*|*/testdata/private/*|*/fixtures/private/*|*/.env|*/.env.*|*/.git/*|*/.git)
      printf 'release guard: forbidden persistent or private path: %s\n' "$path" >&2
      exit 1
      ;;
  esac
  case "$path" in
    *.db|*.db-*|*.db.lock|*.sqlite|*.sqlite-*|*.sqlite3|*.sqlite3-*)
      printf 'release guard: forbidden database artifact: %s\n' "$path" >&2
      exit 1
      ;;
  esac
done

printf 'release guard: checked %d paths (%s)\n' "${#paths[@]}" "$mode"
