#!/usr/bin/env bash
set -euo pipefail

# Exercise checksum/signature/tamper decisions without requiring a private
# signing key. The fake cosign accepts only the signature API; checksum and
# missing-signature failures remain real and fail closed.

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/tokemon-release-integrity.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/dist"
printf 'archive\n' > "$tmp/dist/tokemon_0.0.1_linux_amd64.tar.gz"
printf '{"spdxVersion":"SPDX-2.3"}\n' > "$tmp/dist/sbom.spdx.json"
bash "$root/deploy/update-checksums.sh" "$tmp/dist"
touch "$tmp/dist/checksums.txt.sig" "$tmp/dist/checksums.txt.pem"
touch "$tmp/dist/tokemon_0.0.1_linux_amd64.tar.gz.sig" "$tmp/dist/tokemon_0.0.1_linux_amd64.tar.gz.pem"
touch "$tmp/dist/sbom.spdx.json.sig" "$tmp/dist/sbom.spdx.json.pem"
cat > "$tmp/bin/cosign" <<'EOF'
#!/usr/bin/env bash
[[ "$1" == verify-blob ]] || exit 2
exit 0
EOF
chmod +x "$tmp/bin/cosign"
PATH="$tmp/bin:$PATH" TOKEMON_RELEASE_CERTIFICATE_IDENTITY=test \
  bash "$root/deploy/verify-release.sh" "$tmp/dist" 0.0.1 >/dev/null

printf 'tampered\n' >> "$tmp/dist/tokemon_0.0.1_linux_amd64.tar.gz"
if PATH="$tmp/bin:$PATH" TOKEMON_RELEASE_CERTIFICATE_IDENTITY=test \
  bash "$root/deploy/verify-release.sh" "$tmp/dist" 0.0.1 >/dev/null 2>&1; then
  printf 'tampered archive was accepted\n' >&2
  exit 1
fi
sed -i.bak '/tokemon_0.0.1_linux_amd64.tar.gz/d' "$tmp/dist/checksums.txt"
rm -f "$tmp/dist/tokemon_0.0.1_linux_amd64.tar.gz.sig"
if PATH="$tmp/bin:$PATH" TOKEMON_RELEASE_CERTIFICATE_IDENTITY=test \
  bash "$root/deploy/verify-release.sh" "$tmp/dist" 0.0.1 >/dev/null 2>&1; then
  printf 'unsigned archive was accepted\n' >&2
  exit 1
fi
printf 'release integrity smoke: pass\n'
