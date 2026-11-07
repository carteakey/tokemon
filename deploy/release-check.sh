#!/usr/bin/env bash
set -euo pipefail

# Pre-tag release dry run. Runs the release workflow's gating checks against
# the checked-out candidate commit so a tag is only created on a
# release-ready state: the v0.4.2 tag had to be re-pointed because its image
# failed the container CVE gate after tagging. The release owner checks out
# the candidate commit and runs this script (or dispatches the "Release
# check" workflow, which runs the same script).

candidate="${1:-}"
image="${TOKEMON_CHECK_IMAGE:-tokemon-release-check:dryrun}"
trivy_image="${TOKEMON_CHECK_TRIVY_IMAGE:-aquasec/trivy:0.56.2}"
dryrun_version="${TOKEMON_CHECK_VERSION:-0.0.0-dryrun}"

head_sha="$(git rev-parse HEAD)"
if [[ -n "$candidate" && "$head_sha" != "$candidate"* ]]; then
  printf 'release-check: HEAD %s does not match the requested candidate %s; check out the candidate first\n' "$head_sha" "$candidate" >&2
  exit 2
fi
dirty="$(git status --porcelain)"
if [[ -n "$dirty" ]]; then
  printf 'release-check: the working tree is dirty; a candidate must be clean\n%s\n' "$dirty" >&2
  exit 2
fi
printf 'release-check: candidate %s\n' "$head_sha"

command -v docker >/dev/null 2>&1 || { printf 'release-check: docker is required\n' >&2; exit 127; }
command -v go >/dev/null 2>&1 || { printf 'release-check: go is required\n' >&2; exit 127; }

printf 'release-check[1/4]: headless browser smoke and accessibility regression\n'
TOKEMON_BROWSER_SMOKE=1 go test ./internal/browsercheck -count=1

printf 'release-check[2/4]: build the image with the release build args and assert its identity\n'
docker build \
  --build-arg TOKEMON_VERSION="$dryrun_version" \
  --build-arg TOKEMON_COMMIT="$head_sha" \
  --build-arg TOKEMON_BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --tag "$image" .
image_identity="$(docker run --rm --network none --entrypoint /tokemon "$image" version)"
printf '%s\n' "$image_identity"
grep -Fx "version: $dryrun_version" <<<"$image_identity" || {
  printf 'release-check: image reports the wrong version; expected %s\n' "$dryrun_version" >&2
  exit 1
}
grep -Fx "commit: $head_sha" <<<"$image_identity" || {
  printf 'release-check: image reports the wrong commit; expected %s\n' "$head_sha" >&2
  exit 1
}

printf 'release-check[3/4]: trivy HIGH/CRITICAL container gate\n'
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
docker save "$image" --output "$scratch/tokemon-release.tar"
docker run --rm -v "$scratch:/scan:ro" "$trivy_image" \
  image --input /scan/tokemon-release.tar --exit-code 1 --severity HIGH,CRITICAL --ignore-unfixed

printf 'release-check[4/4]: full test suite with the race detector\n'
go test -race ./...

printf 'release-check: PASS — %s is release-ready; tag it (v<VERSION>) and let the release workflow take over\n' "$head_sha"
