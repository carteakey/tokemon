#!/usr/bin/env bash
set -euo pipefail

# Build the four native agent artifacts consumed by deploy/install-agent.sh.

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
version="${1:-${TOKEMON_VERSION:-}}"
output_dir="${2:-$root/dist}"
version="${version#v}"

[[ -n "$version" ]] || { printf 'usage: build-release.sh VERSION [OUTPUT_DIR]\n' >&2; exit 2; }
[[ "$version" =~ ^[0-9A-Za-z._-]+$ ]] || { printf 'invalid version: %s\n' "$version" >&2; exit 2; }

commit="${TOKEMON_COMMIT:-$(git -C "$root" rev-parse --short HEAD 2>/dev/null || printf 'unknown')}"
build_date="${TOKEMON_BUILD_DATE:-$(date -u '+%Y-%m-%dT%H:%M:%SZ')}"
ldflags="-s -w -X github.com/tokemon/tokemon/internal/version.Version=$version -X github.com/tokemon/tokemon/internal/version.Commit=$commit -X github.com/tokemon/tokemon/internal/version.BuildDate=$build_date"
temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/tokemon-release.XXXXXX")"
trap 'rm -rf "$temporary_dir"' EXIT
mkdir -p "$output_dir"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

for target in "darwin arm64" "darwin amd64" "linux arm64" "linux amd64"; do
  read -r goos goarch <<< "$target"
  artifact="tokemon_${version}_${goos}_${goarch}.tar.gz"
  build_dir="$temporary_dir/$goos-$goarch"
  mkdir -p "$build_dir"
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o "$build_dir/tokemon" "$root/cmd/tokemon"
  tar -C "$build_dir" -czf "$output_dir/$artifact" tokemon
  printf 'built %s\n' "$output_dir/$artifact"
done

checksum_path="$output_dir/checksums.txt"
: > "$checksum_path"
for artifact in "$output_dir"/tokemon_${version}_*.tar.gz; do
  printf '%s  %s\n' "$(sha256_of "$artifact")" "$(basename "$artifact")" >> "$checksum_path"
done
printf 'wrote %s\n' "$checksum_path"
