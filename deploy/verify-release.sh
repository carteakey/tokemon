#!/usr/bin/env bash
set -euo pipefail

# Verify a downloaded release directory before an installer consumes it.
# Cosign's keyless certificate identity and issuer are explicit inputs so a
# release signed by a different workflow cannot be accepted accidentally.

directory="${1:-dist}"
version="${2:-}"
identity="${TOKEMON_RELEASE_CERTIFICATE_IDENTITY:-}"
issuer="${TOKEMON_RELEASE_OIDC_ISSUER:-https://token.actions.githubusercontent.com}"
[[ -d "$directory" ]] || { printf 'release verify: directory not found\n' >&2; exit 2; }
[[ -f "$directory/checksums.txt" ]] || { printf 'release verify: checksums.txt is required\n' >&2; exit 1; }
[[ -f "$directory/checksums.txt.sig" && -f "$directory/checksums.txt.pem" ]] || {
  printf 'release verify: signed checksums are required\n' >&2
  exit 1
}
command -v cosign >/dev/null 2>&1 || { printf 'release verify: cosign is required\n' >&2; exit 127; }

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

if [[ -z "$identity" ]]; then
  [[ -n "$version" ]] || { printf 'release verify: VERSION or TOKEMON_RELEASE_CERTIFICATE_IDENTITY is required\n' >&2; exit 2; }
  identity="https://github.com/carteakey/tokemon/.github/workflows/release.yml@refs/tags/v${version#v}"
fi

verify_blob() {
  local file="$1"
  [[ -f "$file.sig" && -f "$file.pem" ]] || { printf 'release verify: unsigned asset: %s\n' "$(basename "$file")" >&2; exit 1; }
  cosign verify-blob \
    --certificate "$file.pem" \
    --signature "$file.sig" \
    --certificate-identity "$identity" \
    --certificate-oidc-issuer "$issuer" \
    "$file" >/dev/null
}

verify_checksum_set() {
  local checksum_file="$1" expected file actual
  while read -r expected file; do
    [[ -n "$expected" && -n "$file" ]] || continue
    file="${file#\*}"
    [[ -f "$file" ]] || { printf 'release verify: missing asset: %s\n' "$file" >&2; exit 1; }
    actual="$(sha256_of "$file")"
    [[ "$actual" == "$expected" ]] || { printf 'release verify: checksum mismatch: %s\n' "$file" >&2; exit 1; }
  done < "$checksum_file"
}

verify_blob "$directory/checksums.txt"
(cd "$directory" && verify_checksum_set checksums.txt)
if [[ -f "$directory/image-digest.txt" ]]; then
  verify_blob "$directory/image-digest.txt"
fi
for archive in "$directory"/tokemon_*.tar.gz; do
  [[ -f "$archive" ]] || continue
  archive_name="$(basename "$archive")"
  grep -Eq "^[[:xdigit:]]{64}[[:space:]]+[*]?${archive_name//./\\.}$" "$directory/checksums.txt" || {
    printf 'release verify: archive missing from signed checksums: %s\n' "$archive_name" >&2
    exit 1
  }
  verify_blob "$archive"
done
while IFS= read -r artifact; do
  [[ -n "$artifact" ]] || continue
  [[ "$artifact" == tokemon_*.tar.gz ]] && continue
  verify_blob "$directory/$artifact"
done < <(awk '{print $2}' "$directory/checksums.txt")
printf 'release verify: checksums and signatures valid\n'
