#!/bin/sh
# Rigscope installer: download the release archive, verify it, and hand over to
# `rigscope install`. POSIX sh, no root, no dependencies beyond curl/wget, tar
# and a SHA-256 tool. Set RIGSCOPE_VERSION=1.2.3 to pin a release.
#
# RIGSCOPE_INSTALL_BASE_URL overrides the release download root. It exists for
# the end-to-end test that serves a real archive from a local HTTPS server; it
# is not part of the documented user interface.
set -eu

REPO="Triet1402/rigscope"
BASE="https://github.com/${REPO}/releases"
if [ -n "${RIGSCOPE_INSTALL_BASE_URL:-}" ]; then
	BASE=${RIGSCOPE_INSTALL_BASE_URL%/}
fi

err() {
	printf 'rigscope-install: %s\n' "$*" >&2
	exit 1
}

[ "$(uname -s)" = "Linux" ] || err "Rigscope supports Linux only"

case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	armv7l | armv7) arch=armv7 ;;
	*) err "unsupported architecture $(uname -m); Rigscope supports x86_64, aarch64 and armv7l" ;;
esac

command -v tar >/dev/null 2>&1 || err "tar is required"

tmp=$(mktemp -d "${TMPDIR:-/tmp}/rigscope-install.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

fetch() {
	url=$1
	dest=$2
	stage=$3
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --proto '=https' --tlsv1.2 \
			--connect-timeout 15 --max-time 300 \
			-o "$dest" "$url" ||
			err "download failed (${stage}): ${url}"
	elif command -v wget >/dev/null 2>&1 && wget --version 2>/dev/null | grep -q 'GNU Wget'; then
		# GNU wget verifies certificates and supports --https-only. BusyBox
		# wget is refused below: depending on how it was built it may not
		# verify TLS certificates at all, and the checksum file comes over the
		# same connection as the archive.
		# --timeout bounds connect and read stalls; timeout(1) bounds the whole
		# transfer when it is available.
		if command -v timeout >/dev/null 2>&1; then
			timeout 300 wget -q --https-only --timeout=15 --tries=1 \
				-O "$dest" "$url" ||
				err "download failed (${stage}): ${url}"
		else
			wget -q --https-only --timeout=15 --tries=1 \
				-O "$dest" "$url" ||
				err "download failed (${stage}): ${url}"
		fi
	else
		err "curl or GNU wget is required (BusyBox wget is not supported because it may not verify TLS certificates); install curl and run the installer again"
	fi
}

if [ -n "${RIGSCOPE_VERSION:-}" ]; then
	version=${RIGSCOPE_VERSION#v}
	tag="v${version}"
else
	fetch "https://api.github.com/repos/${REPO}/releases/latest" "$tmp/release.json" "latest release metadata"
	tag=$(sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' "$tmp/release.json" | head -n 1)
	[ -n "$tag" ] || err "could not determine the latest Rigscope release"
	version=${tag#v}
fi

asset="rigscope_${version}_linux_${arch}.tar.gz"
fetch "${BASE}/download/${tag}/${asset}" "$tmp/${asset}" "${asset}"
fetch "${BASE}/download/${tag}/checksums.txt" "$tmp/checksums.txt" "checksums.txt"

expected=$(awk -v name="$asset" '$2 == name || $2 == ("*" name) { print $1; exit }' "$tmp/checksums.txt")
[ -n "$expected" ] || err "checksums.txt from ${BASE}/download/${tag}/checksums.txt has no entry for ${asset}"
if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$tmp/${asset}" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
	actual=$(shasum -a 256 "$tmp/${asset}" | awk '{ print $1 }')
else
	err "sha256sum or shasum is required"
fi
[ "$actual" = "$expected" ] ||
	err "checksum mismatch for ${asset} downloaded from ${BASE}/download/${tag}/${asset}: expected ${expected}, got ${actual}; refusing to install"

# List the members with tar -tzf instead of parsing tar -tv output: GNU tar
# prints the size in field 3 while POSIX tar prints it in field 5, so a
# size check based on -tv silently rejects valid GNU archives. The release
# archive contains exactly one member, the `rigscope` binary, and its name is a
# plain basename (no directories, no parent components).
listing=$(tar -tzf "$tmp/${asset}" 2>/dev/null) || err "could not list ${asset}"
printf '%s\n' "$listing" >"$tmp/members"
member_raw=
member_count=0
while IFS= read -r name; do
	[ -n "$name" ] || continue
	case "$name" in
		rigscope | ./rigscope) ;;
		*) err "archive ${asset} contains an unexpected member ${name}" ;;
	esac
	member_raw=$name
	member_count=$((member_count + 1))
done <"$tmp/members"
rm -f "$tmp/members"
[ "$member_count" -eq 1 ] || err "archive ${asset} must contain exactly one member named rigscope"
[ -n "$member_raw" ] || err "archive ${asset} does not contain rigscope"

mkdir "$tmp/extract"
tar -xzf "$tmp/${asset}" -C "$tmp/extract" "$member_raw" || err "could not extract rigscope from ${asset}"
[ -f "$tmp/extract/rigscope" ] && [ ! -L "$tmp/extract/rigscope" ] || err "the extracted rigscope member is not a regular file"
unpacked=$(wc -c <"$tmp/extract/rigscope" | tr -d ' ')
[ "$unpacked" -le 209715200 ] || err "the unpacked rigscope binary is larger than 200 MB; refusing to run it"
rm -f "$tmp/${asset}" "$tmp/checksums.txt" "$tmp/release.json"

# Run the installer as a child, not with exec, so the EXIT trap removes the
# temporary directory even when the installer fails.
if "$tmp/extract/rigscope" install "$@"; then
	exit 0
else
	status=$?
	exit "$status"
fi
