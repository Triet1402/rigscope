#!/bin/sh
# Rigscope 1.0 installer lifecycle end-to-end test.
#
# Builds two real releases with scripts/release/build.sh, serves them from a
# local HTTPS server with a test CA, and drives install -> enrollment ->
# upgrade -> rollback -> uninstall with the real installed binaries. Every
# system interaction goes through a fake systemctl/journalctl/loginctl on PATH
# and a disposable HOME/XDG tree, so the live installation on this machine is
# never touched.
#
# Usage:
#   sh scripts/e2e/lifecycle.sh
#
# Environment:
#   RIGSCOPE_E2E_KEEP=1   Keep the temporary work directory for debugging.
#   TMPDIR                Parent for the disposable directory (default /tmp/opencode).
#
# The script is re-runnable: each run gets a new temporary directory and the
# EXIT trap stops any fake service left behind and removes the tree.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)

case ":$PATH:" in
	*":$HOME/.local/bin:"*) ;;
	*) PATH="$HOME/.local/bin:$PATH"; export PATH ;;
esac

command -v go >/dev/null 2>&1 || {
	printf 'e2e: go is required (Go 1.27)\n' >&2
	exit 1
}

parent=${TMPDIR:-/tmp/opencode}
mkdir -p "$parent"
work=$(mktemp -d "$parent/rigscope-e2e.XXXXXX")
cleanup_done=0
cleanup() {
	[ "$cleanup_done" -eq 0 ] || return 0
	cleanup_done=1
	# Stop fake services directly as a backstop: the orchestrator does this on
	# its normal path, but an interrupt or panic must not leave processes.
	for pidfile in "$work"/*/fake-systemd/run/*.pid; do
		[ -f "$pidfile" ] || continue
		pid=$(sed -n '1p' "$pidfile" 2>/dev/null || true)
		case "$pid" in
			'' | *[!0-9]*) continue ;;
		esac
		kill -TERM "$pid" 2>/dev/null || true
	done
	sleep 1 2>/dev/null || true
	for pidfile in "$work"/*/fake-systemd/run/*.pid; do
		[ -f "$pidfile" ] || continue
		pid=$(sed -n '1p' "$pidfile" 2>/dev/null || true)
		case "$pid" in
			'' | *[!0-9]*) continue ;;
		esac
		kill -KILL "$pid" 2>/dev/null || true
	done
	if [ "${RIGSCOPE_E2E_KEEP:-0}" = "1" ]; then
		printf 'e2e: kept work directory %s\n' "$work" >&2
	else
		rm -rf -- "$work"
	fi
}
trap cleanup EXIT HUP INT TERM

printf 'e2e: work directory %s\n' "$work"
printf 'e2e: building orchestrator and fake systemd tools\n'

(
	cd "$root"
	go build -trimpath -o "$work/orchestrator" ./scripts/e2e
)

printf 'e2e: running lifecycle\n'
"$work/orchestrator" -root "$root" -work "$work"
status=$?
if [ "$status" -ne 0 ]; then
	printf 'e2e: lifecycle failed (exit %d); logs were dumped above\n' "$status" >&2
fi
exit "$status"
