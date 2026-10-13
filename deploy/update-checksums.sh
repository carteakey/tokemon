#!/usr/bin/env bash
set -euo pipefail

directory="${1:-dist}"
[[ -d "$directory" ]] || { printf 'checksum update: directory not found: %s\n' "$directory" >&2; exit 2; }

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

artifacts=()
while IFS= read -r artifact; do artifacts+=("$artifact"); done < <(find "$directory" -maxdepth 1 -type f \( \
  -name 'tokemon_*.tar.gz' -o -name 'sbom.spdx.json' \
  \) -print | sort)
(( ${#artifacts[@]} > 0 )) || { printf 'checksum update: no release artifacts found\n' >&2; exit 1; }

checksum_path="$directory/checksums.txt"
temporary="$checksum_path.tmp"
trap 'rm -f "$temporary"' EXIT
: > "$temporary"
for artifact in "${artifacts[@]}"; do
  printf '%s  %s\n' "$(sha256_of "$artifact")" "$(basename "$artifact")" >> "$temporary"
done
mv -f "$temporary" "$checksum_path"
printf 'checksum update: wrote %s (%d artifacts)\n' "$checksum_path" "${#artifacts[@]}"
