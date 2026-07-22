#!/usr/bin/env bash
set -euo pipefail

# Keep the historical macOS entry point, but route all agent installs through
# the shared installer so release downloads, config handling, and launchd
# behavior stay identical across macOS and Linux.
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
exec "$script_dir/../install-agent.sh" "$@"
