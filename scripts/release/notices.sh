#!/bin/sh
# Generate THIRD_PARTY_NOTICES-go.md from the local Go module graph.
#
# Usage: scripts/release/notices.sh [output-file]
#
# Reads the standard library and every module in `go list -m -json all`, then
# copies the license files found in the module cache. The network is disabled
# (GOPROXY=off) so a missing module is an error, never a download. On a fresh
# checkout run `go mod download` once first (scripts/release/build.sh does).
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
output=${1:-"$root/THIRD_PARTY_NOTICES-go.md"}

case "$output" in
	/*) ;;
	*) output="$root/$output" ;;
esac

cd "$root"
GOPROXY=off go run ./scripts/release/notices -root "$root" -o "$output"
printf 'wrote %s\n' "$output"
