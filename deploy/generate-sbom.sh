#!/usr/bin/env bash
set -euo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
output="${1:-$root/dist/sbom.spdx.json}"
command -v syft >/dev/null 2>&1 || {
  printf 'SBOM generation requires syft (https://github.com/anchore/syft)\n' >&2
  exit 127
}
mkdir -p "$(dirname "$output")"
syft "dir:$root" \
  --exclude './data/**' \
  --exclude '**/*.db' \
  --exclude '**/*.db-*' \
  --exclude '**/*.sqlite*' \
  --exclude './incidents/**' \
  --output "spdx-json=$output"
printf 'SBOM: %s\n' "$output"
