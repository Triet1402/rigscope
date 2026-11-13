#!/bin/sh
# Verify the Rigscope 1.0 release artifacts.
#
# Usage: scripts/release/verify.sh <version> [artifact-directory]
#
# Checks, in order:
#   1. the artifact set is exactly what build.sh publishes;
#   2. checksums.txt is strictly formatted and every checksum matches;
#   3. every tarball contains exactly one regular member named `rigscope`
#      (the rule install.sh and internal/release enforce);
#   4. the amd64 binary prints the expected version;
#   5. the hub starts with a temporary config on a random loopback port and `/`
#      returns 200 HTML, proving the UI export is embedded;
#   6. arm64 and armv7 binaries are little-endian ELF for the right machine;
#   7. arm64 and armv7 execute `version` and the hub smoke under qemu-user when
#      a qemu runner is present; otherwise the verifier says they were not
#      checked, and docs/rewrite/releasing-v1.md requires the manual run;
#   8. install.sh matches the script in the tree and both notices assets match
#      the generated files;
#   9. scripts/install.sh installs from a local HTTPS server into a fake HOME
#      with fake systemctl commands, and the installed binary reports the
#      release version.
#
# This script uses only the produced artifacts plus the local Go toolchain for
# the two HTTP smoke tests. It publishes nothing.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
version=${1:-}
dir=${2:-}

usage() {
	printf 'usage: %s <version> [artifact-directory]\n' "$0" >&2
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
if [ -z "$dir" ]; then
	dir="$root/build/release/v$version"
elif [ "${dir#/}" = "$dir" ]; then
	dir="$root/$dir"
fi

fail() {
	printf 'verify: %s\n' "$*" >&2
	exit 1
}

[ -d "$dir" ] || fail "artifact directory $dir does not exist"

tmp=$(mktemp -d "${TMPDIR:-/tmp}/rigscope-verify.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"
command -v od >/dev/null 2>&1 || fail "od is required"
command -v go >/dev/null 2>&1 || fail "go is required for the HTTP smoke tests"

assets="rigscope_${version}_linux_amd64.tar.gz rigscope_${version}_linux_arm64.tar.gz rigscope_${version}_linux_armv7.tar.gz"
expected="LICENSE
THIRD_PARTY_NOTICES-go.md
THIRD_PARTY_NOTICES.md
checksums.txt
install.sh
rigscope_${version}_linux_amd64.tar.gz
rigscope_${version}_linux_arm64.tar.gz
rigscope_${version}_linux_armv7.tar.gz
rigscope_${version}_source.tar.gz"
actual=$(cd "$dir" && LC_ALL=C ls -A | LC_ALL=C sort)
[ "$actual" = "$expected" ] || fail "unexpected artifact set in $dir:
$actual"

# checksums.txt: every non-empty line is "<64 lowercase hex>  <name>". An
# asterisk form is accepted by internal/release but build.sh never writes it.
awk 'NF == 0 { next } NF != 2 { exit 1 } length($1) != 64 || $1 !~ /^[0-9a-f]+$/ { exit 1 }' \
	"$dir/checksums.txt" || fail "checksums.txt is not in the strict format"
for name in $assets install.sh LICENSE "rigscope_${version}_source.tar.gz" THIRD_PARTY_NOTICES-go.md THIRD_PARTY_NOTICES.md; do
	awk -v name="$name" '$2 == name { found = 1 } END { exit found ? 0 : 1 }' \
		"$dir/checksums.txt" || fail "checksums.txt has no entry for $name"
done
(cd "$dir" && sha256sum -c checksums.txt >/dev/null) || fail "checksum verification failed"

cmp -s "$dir/install.sh" "$root/scripts/install.sh" || fail "install.sh asset differs from scripts/install.sh"
cmp -s "$dir/THIRD_PARTY_NOTICES-go.md" "$root/THIRD_PARTY_NOTICES-go.md" ||
	fail "THIRD_PARTY_NOTICES-go.md asset is stale; run scripts/release/notices.sh"
cmp -s "$dir/THIRD_PARTY_NOTICES.md" "$root/THIRD_PARTY_NOTICES.md" ||
	fail "THIRD_PARTY_NOTICES.md asset is stale; run npm run notices"

cmp -s "$dir/LICENSE" "$root/LICENSE" || fail "release LICENSE is stale"
source_prefix="rigscope-$version"
tar -xOf "$dir/rigscope_${version}_source.tar.gz" "$source_prefix/LICENSE" >"$tmp/source-license"
cmp -s "$tmp/source-license" "$root/LICENSE" || fail "source archive LICENSE is stale"
for member in go.mod go.sum package-lock.json scripts/release/build.sh vendor/modules.txt third-party-ui/inventory.json; do
 tar -tzf "$dir/rigscope_${version}_source.tar.gz" "$source_prefix/$member" >/dev/null || fail "source archive missing $member"
done

# Every binary tarball must contain exactly one member named rigscope, and it must be a
# regular executable file. install.sh refuses anything else before extraction.
extract() {
	arch=$1
	asset="rigscope_${version}_linux_${arch}.tar.gz"
	members="$tmp/$arch.members"
	tar -tzf "$dir/$asset" >"$members" 2>/dev/null || fail "could not list $asset"
	count=0
	while IFS= read -r name; do
		[ -n "$name" ] || continue
		case "$name" in
			rigscope) ;;
			*) fail "$asset contains an unexpected member $name" ;;
		esac
		count=$((count + 1))
	done <"$members"
	[ "$count" -eq 1 ] || fail "$asset must contain exactly one member named rigscope (found $count)"
	dest="$tmp/$arch"
	mkdir "$dest"
	tar -xzf "$dir/$asset" -C "$dest" rigscope || fail "could not extract $asset"
	[ -f "$dest/rigscope" ] || fail "$asset did not extract a regular rigscope file"
	[ ! -L "$dest/rigscope" ] || fail "$asset extracted rigscope as a symlink"
	[ -x "$dest/rigscope" ] || fail "$asset extracted a non-executable rigscope"
}

check_elf() {
	arch=$1
	class=$2
	machine=$3
	file="$tmp/$arch/rigscope"
	magic=$(od -An -tc -N4 "$file" | tr -d ' \n')
	[ "$magic" = "177ELF" ] || fail "$arch binary is not an ELF file"
	got_class=$(od -An -tu1 -j4 -N1 "$file" | tr -d ' ')
	got_data=$(od -An -tu1 -j5 -N1 "$file" | tr -d ' ')
	got_machine=$(od -An -tu1 -j18 -N2 "$file" | awk '{ print $1 + $2 * 256 }')
	[ "$got_class" = "$class" ] || fail "$arch ELF class is $got_class, expected $class"
	[ "$got_data" = "1" ] || fail "$arch ELF byte order is $got_data, expected little-endian"
	[ "$got_machine" = "$machine" ] || fail "$arch ELF machine is $got_machine, expected $machine"
}

for arch in amd64 arm64 armv7; do
	extract "$arch"
done
check_elf amd64 2 62
check_elf arm64 2 183
check_elf armv7 1 40

reported=$("$tmp/amd64/rigscope" version | awk '{ print $NF }')
[ "$reported" = "$version" ] || fail "amd64 binary reports version $reported, expected $version"
"$tmp/amd64/rigscope" version || fail "amd64 binary could not run version"

"$tmp/amd64/rigscope" license >"$tmp/embedded-license"
tail -n +2 "$tmp/embedded-license" >"$tmp/embedded-terms"
cmp -s "$tmp/embedded-terms" "$root/LICENSE" || fail "binary does not embed the release license"

printf 'verify: checking the embedded UI with a temporary hub\n'
(cd "$root" && go run ./scripts/release/check -mode hub -binary "$tmp/amd64/rigscope" -version "$version") ||
	fail "hub smoke test failed"

# PLAN phase 3 wants execution evidence per architecture, not just an ELF
# header. qemu-user (with binfmt_misc or invoked directly) executes the cross-
# built binaries when it happens to be installed; verify.sh never installs it.
# Without it the ARM binaries stay unexecuted, which the notice and the manual
# step in docs/rewrite/releasing-v1.md make explicit.
qemu_runner() {
	case "$1" in
		arm64)
			command -v qemu-aarch64 2>/dev/null ||
				command -v qemu-aarch64-static 2>/dev/null
			;;
		armv7)
			command -v qemu-arm 2>/dev/null ||
				command -v qemu-arm-static 2>/dev/null
			;;
	esac
}

for arch in arm64 armv7; do
	runner=$(qemu_runner "$arch") || runner=""
	if [ -z "$runner" ]; then
		printf 'verify: NOTICE: %s execution was NOT checked: no qemu-%s runner is installed\n' \
			"$arch" "$([ "$arch" = arm64 ] && printf aarch64 || printf arm)"
		printf 'verify: NOTICE: run `%s version` and a hub boot on real %s hardware, or under qemu, before publishing\n' \
			"rigscope" "$arch"
		continue
	fi
	printf 'verify: executing the %s binary under %s\n' "$arch" "$runner"
	reported=$("$runner" "$tmp/$arch/rigscope" version | awk '{ print $NF }')
	[ "$reported" = "$version" ] || fail "qemu $arch binary reports version $reported, expected $version"
	(cd "$root" && go run ./scripts/release/check -mode hub -qemu "$runner" \
		-binary "$tmp/$arch/rigscope" -version "$version") ||
		fail "hub smoke test failed under qemu for $arch"
done

printf 'verify: installing from a local HTTPS server into a fake HOME\n'
(cd "$root" && go run ./scripts/release/check -mode install -root "$root" -dir "$dir" -version "$version") ||
	fail "install.sh smoke test failed"

printf 'verify: artifact sizes\n'
for asset in $assets install.sh LICENSE "rigscope_${version}_source.tar.gz" THIRD_PARTY_NOTICES-go.md THIRD_PARTY_NOTICES.md checksums.txt; do
	printf '  %8s bytes  %s\n' "$(wc -c <"$dir/$asset" | tr -d ' ')" "$asset"
done
printf '  %8s bytes  %s\n' "$(wc -c <"$tmp/amd64/rigscope" | tr -d ' ')" "rigscope (amd64, extracted)"

printf 'verify: OK (version %s)\n' "$version"
