#!/bin/sh
# Build the Rigscope 1.0 release artifacts reproducibly.
#
# Usage: scripts/release/build.sh <version> [output-directory]
#
# The version is written into the binary (`rigscope version` prints it) and into
# every asset name. A leading "v" is accepted and stripped. The output directory
# defaults to build/release/v<version> and is replaced on every run.
#
# Builds the UI export and embeds it once, then cross-compiles linux/amd64,
# linux/arm64 and linux/armv7 with CGO disabled. Every tarball contains exactly
# one member, `rigscope`, because scripts/install.sh and internal/release both
# require that. install.sh and the Go and UI third-party notices are separate
# assets.
#
# Reproducibility: -trimpath, -buildvcs=false, a fixed SOURCE_DATE_EPOCH (the
# HEAD commit time unless overridden), sorted tar entries with fixed ownership
# and mtime, and gzip with no name or timestamp. Building the same revision
# twice produces identical sha256 sums.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
version=${1:-}
out=${2:-}

usage() {
	printf 'usage: %s <version> [output-directory]\n' "$0" >&2
	exit 2
}

[ -n "$version" ] || usage
case "$version" in
	v*) version=${version#v} ;;
esac
case "$version" in
	'' | . | ..) usage ;;
	*[!0-9A-Za-z.+-]*) usage ;;
	[0-9A-Za-z]*) ;;
	*) usage ;;
esac

if [ -z "$out" ]; then
	out="$root/build/release/v$version"
elif [ "${out#/}" = "$out" ]; then
	out="$root/$out"
fi
case "$out" in
	/) printf 'build: refusing to replace /\n' >&2; exit 1 ;;
esac

cd "$root"

command -v go >/dev/null 2>&1 || { printf 'build: go is required\n' >&2; exit 1; }
command -v npm >/dev/null 2>&1 || { printf 'build: npm is required\n' >&2; exit 1; }
command -v tar >/dev/null 2>&1 || { printf 'build: tar is required\n' >&2; exit 1; }
command -v gzip >/dev/null 2>&1 || { printf 'build: gzip is required\n' >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || { printf 'build: sha256sum is required\n' >&2; exit 1; }

tar --version 2>/dev/null | grep -q 'GNU tar' || {
	printf 'build: GNU tar is required for deterministic archives\n' >&2
	exit 1
}

if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
	if SOURCE_DATE_EPOCH=$(git -C "$root" log -1 --format=%ct 2>/dev/null); then
		:
	else
		SOURCE_DATE_EPOCH=0
	fi
fi
case "$SOURCE_DATE_EPOCH" in
	'' | *[!0-9]*) printf 'build: SOURCE_DATE_EPOCH must be an integer\n' >&2; exit 1 ;;
esac
export SOURCE_DATE_EPOCH

# A fresh checkout has no modules in the cache. Fetch the build list once; the
# notices generator below runs with GOPROXY=off and only reads that cache.
printf 'build: ensuring Go modules are in the cache\n'
go mod download

# Both notices files are generated, committed artifacts. Regenerate them before
# anything is embedded so the release always matches the module graph and the
# installed UI dependencies.
printf 'build: generating Go third-party notices\n'
sh scripts/release/notices.sh "$root/THIRD_PARTY_NOTICES-go.md"
printf 'build: generating UI third-party notices\n'
npm run notices

# One UI build for all architectures. `go generate` copies the static export
# into internal/hub/webui/dist and writes the CSP hash manifest.
printf 'build: building the UI export\n'
NEXT_PUBLIC_RIGSCOPE_VERSION="$version" npm run build:ui
# Next embeds a random build id, which would make the embedded export (and the
# binary) differ between two builds of the same revision. Replace it with a
# content-derived id before the export is embedded.
go run ./scripts/release/canonicalize -dir out
go generate ./internal/hub/webui
if [ ! -f internal/hub/webui/dist/index.html ]; then
	printf 'build: the embedded UI export is missing index.html\n' >&2
	exit 1
fi

printf 'build: replacing %s\n' "$out"
rm -rf -- "$out"
mkdir -p -- "$out"
stage=$(mktemp -d "$out/.stage.XXXXXX")
cleanup() {
	rm -rf -- "$stage"
}
trap cleanup EXIT HUP INT TERM

ldflags="-s -w -X github.com/Triet1402/rigscope/internal/version.Version=$version"

build_one() {
	goarch=$1
	arch=$2
	goarm=$3
	asset="rigscope_${version}_linux_${arch}.tar.gz"
	dir="$stage/$arch"
	mkdir -p "$dir"

	if [ -n "$goarm" ]; then
		CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" GOARM="$goarm" \
			go build -trimpath -buildvcs=false -ldflags "$ldflags" \
			-o "$dir/rigscope" ./cmd/rigscope
	else
		CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
			go build -trimpath -buildvcs=false -ldflags "$ldflags" \
			-o "$dir/rigscope" ./cmd/rigscope
	fi

	chmod 0755 "$dir/rigscope"
	touch -d "@$SOURCE_DATE_EPOCH" "$dir/rigscope"
	# GNU tar: sorted members, fixed owner and mtime, POSIX ustar format; gzip
	# stores no original name or timestamp. The archive holds exactly one
	# member, which install.sh and internal/release require.
	(
		cd "$dir"
		tar --sort=name --format=ustar --mtime="@$SOURCE_DATE_EPOCH" \
			--owner=0 --group=0 --numeric-owner -cf - rigscope
	) | gzip -n -9 >"$out/$asset"
	printf 'build: %s (%s bytes, binary %s bytes)\n' "$asset" \
		"$(wc -c <"$out/$asset" | tr -d ' ')" \
		"$(wc -c <"$dir/rigscope" | tr -d ' ')"
}

printf 'build: cross-compiling linux/amd64 linux/arm64 linux/armv7\n'
build_one amd64 amd64 ""
build_one arm64 arm64 ""
build_one arm armv7 7

cp scripts/install.sh "$out/install.sh"
chmod 0755 "$out/install.sh"
cp THIRD_PARTY_NOTICES-go.md "$out/THIRD_PARTY_NOTICES-go.md"
cp THIRD_PARTY_NOTICES.md "$out/THIRD_PARTY_NOTICES.md"
cp LICENSE "$out/LICENSE"

printf 'build: packaging corresponding source and dependency sources\n'
source_name="rigscope_${version}_source.tar.gz"
source_root="$stage/rigscope-$version"
node scripts/release/source.cjs "$source_root"
(
 cd "$stage"
 tar --sort=name --format=posix --pax-option=delete=atime,delete=ctime \
   --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner \
   -cf - "rigscope-$version"
) | gzip -n -9 >"$out/$source_name"

# Strict checksums.txt format: "<64 lowercase hex>  <name>", one asset per
# line, in a fixed order. internal/release parses this format strictly and
# install.sh reads it with awk.
(
	cd "$out"
	printf '%s\n' \
		"rigscope_${version}_linux_amd64.tar.gz" \
		"rigscope_${version}_linux_arm64.tar.gz" \
		"rigscope_${version}_linux_armv7.tar.gz" \
		"install.sh" \
        "LICENSE" \
        "$source_name" \
		"THIRD_PARTY_NOTICES-go.md" \
		"THIRD_PARTY_NOTICES.md" |
		LC_ALL=C sort |
		while IFS= read -r name; do
			printf '%s  %s\n' "$(sha256sum "$name" | awk '{ print $1 }')" "$name"
		done >checksums.txt
)

printf 'build: artifacts in %s\n' "$out"
(
	cd "$out"
	LC_ALL=C ls -l LICENSE "$source_name" checksums.txt install.sh THIRD_PARTY_NOTICES-go.md THIRD_PARTY_NOTICES.md \
		"rigscope_${version}_linux_amd64.tar.gz" \
		"rigscope_${version}_linux_arm64.tar.gz" \
		"rigscope_${version}_linux_armv7.tar.gz"
)
