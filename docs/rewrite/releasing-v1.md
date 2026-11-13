# Releasing Rigscope 1.0

Rigscope 1.0 is one static Go binary for Linux on `amd64`, `arm64` and `armv7`.
Rigscope project sources are AGPL-3.0-only; third-party notices keep their
own licenses. A release is a set of GitHub Release assets, the installer served from
`https://rigscope.dev/install.sh`, and (once) an npm deprecation notice telling
0.2 users where to go. There is no npm publication, no container image and no
runtime download in 1.0.

Nothing in this repository publishes automatically. The manual CI workflow and
the manual release workflow only build and verify; they upload workflow
artifacts. Pushing a tag, creating the GitHub Release, deploying the website and
deprecating npm are separate operator actions described below. Do not let a
workflow, script or test perform them.

## Version scheme

- Release versions are `MAJOR.MINOR.PATCH` without a leading `v`: `1.0.0`.
- The Git tag is `v` + the version: `v1.0.0`.
- The version is compiled in with `-X internal/version.Version=<version>` by
  `scripts/release/build.sh`; no source file needs editing for a release.
- `rigscope version` prints `rigscope <version>`. `internal/release` and
  `scripts/install.sh` both compare that output with the release tag, so a
  mismatched build is rejected during upgrade or installation.
- Asset names embed the version exactly:
  `rigscope_<version>_linux_<arch>.tar.gz`, with `<arch>` one of `amd64`,
  `arm64` or `armv7`.

## Release assets

| Asset | Contents |
| --- | --- |
| `rigscope_<version>_linux_amd64.tar.gz` | exactly one member, `rigscope` (regular, executable) |
| `rigscope_<version>_linux_arm64.tar.gz` | same, `GOARCH=arm64` |
| `rigscope_<version>_linux_armv7.tar.gz` | same, `GOARCH=arm`, `GOARM=7` |
| `checksums.txt` | strict `<64 lowercase hex><two spaces><name>` lines for the three tarballs, `install.sh`, and both notices files, `LICENSE` and the source archive |
| `install.sh` | the canonical POSIX installer, byte-identical to `scripts/install.sh` |
| `THIRD_PARTY_NOTICES-go.md` | Go standard library and Go module licenses |
| `THIRD_PARTY_NOTICES.md` | npm/UI licenses for the vendor bundle embedded in the binary |
| `LICENSE` | GNU AGPL version 3 terms |
| `rigscope_<version>_source.tar.gz` | committed project sources and dependency sources |

The tarballs deliberately contain exactly one member. `scripts/install.sh`
refuses any other member, and `internal/release` treats an archive with a
second `rigscope` member as invalid. Licenses therefore ship as separate
assets, not inside the tarballs. The UI notices cover the npm packages and
Next.js runtime bundled into the embedded static export; the Go notices cover
the compiled modules.

The release also includes `LICENSE` (AGPL-3.0-only) and
`rigscope_<version>_source.tar.gz`. The source archive contains the exact
committed tree, vendored Go modules and the installed browser dependency
packages. Both are covered by `checksums.txt`. There are nine release assets.
The binary tarballs still contain exactly one executable.

## Prerequisites

- Linux x86_64 release host with GNU tar, `sha256sum`, `gzip` and `od`.
- Go 1.27.4 (`go version`).
- Node 26.13.0 and npm (`node --version`). Install the locked root dependencies
  once with `npm ci`; the UI build needs `node_modules`.
- No network access is needed during `build.sh`/`verify.sh` once Go modules and
  `node_modules` are present. `build.sh` runs `go mod download` for a fresh
  checkout; the notices generator itself runs with `GOPROXY=off` and never
  fetches anything.

## 1. Preflight

1. Confirm the revision is the one you intend to release:

   ```sh
   git status --short          # must be clean
   git log -1 --format='%H %ct %s'
   ```

2. Run the manual CI workflow on that revision from the GitHub Actions UI
   (workflow "CI", `workflow_dispatch`). CI is manual-only in this repository;
   pushes and pull requests do not trigger it. It runs `go vet ./...`,
   `go test ./...`, the UI checks, and the release build plus verifier.

3. If the release changes dependencies, regenerate and commit both notices
   files before building (the release build also regenerates them):

   ```sh
   sh scripts/release/notices.sh        # THIRD_PARTY_NOTICES-go.md
   npm run notices                      # THIRD_PARTY_NOTICES.md (UI bundle)
   git add THIRD_PARTY_NOTICES-go.md THIRD_PARTY_NOTICES.md
   ```

## 2. Build the artifacts

```sh
sh scripts/release/build.sh 1.0.0
```

This regenerates both third-party notices files (Go and UI) first, then runs the
UI build and `go generate ./internal/hub/webui` once, then cross-compiles all
three architectures with `CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false` and
`-ldflags "-s -w -X
github.com/Triet1402/rigscope/internal/version.Version=1.0.0"`. Artifacts go to
`build/release/v1.0.0/` (git-ignored). Use the optional second argument to write
elsewhere:

```sh
sh scripts/release/build.sh 1.0.0 build/release/my-candidate
```

Next.js stamps a random build id into its static export. `build.sh` runs
`scripts/release/canonicalize` after `npm run build:ui` and before `go generate`
to replace it with a content-derived id, so the export is stable build to build.
The tarballs use GNU tar with sorted members, fixed owner/group, fixed mtime and
`gzip -n`; `SOURCE_DATE_EPOCH` defaults to the HEAD commit time and can be
overridden for testing.

## 3. Verify the exact artifacts

```sh
sh scripts/release/verify.sh 1.0.0
```

The verifier checks, from the produced files only:

- the artifact set is exactly the nine assets in the table above;
- `checksums.txt` uses the strict format and `sha256sum -c` passes;
- each tarball contains exactly one regular member named `rigscope`;
- the amd64 binary runs `version` and prints the release version;
- the hub starts with a temporary config on a random loopback port and `GET /`
  returns 200 HTML, proving the UI export is embedded;
- arm64 and armv7 are little-endian ELF files with the expected class and
  machine type;
- arm64 and armv7 run `version` and the same hub boot smoke under `qemu-aarch64`
  / `qemu-arm` (or their static variants) when a qemu-user runner is installed;
  otherwise the verifier prints a loud "ARM execution was NOT checked" notice
  and the manual step in [ARM execution evidence](#arm-execution-evidence)
  applies;
- `install.sh` matches `scripts/install.sh` and both notices assets match the
  generated files;
- `scripts/install.sh` installs the amd64 tarball from a local HTTPS server with
  a dedicated test CA into a fake `HOME` with fake `systemctl` commands, and the
  installed binary reports the release version.

### ARM execution evidence

PLAN phase 3 requires an execution smoke test per architecture, not just an ELF
header check. The release host usually has no ARM hardware, so do one of these
and record the result with the release evidence:

```sh
# With qemu-user installed (verify.sh does this automatically when present):
qemu-aarch64 ./build/release/v1.0.0/rigscope version   # after extracting the arm64 tarball
qemu-arm     ./build/release/v1.0.0/rigscope version    # after extracting the armv7 tarball
```

Without qemu, run the tarball on real hardware (or a native ARM machine/VM):

```sh
tar -xzf rigscope_1.0.0_linux_arm64.tar.gz
./rigscope version
./rigscope install --role hub --no-start   # then start it once and check GET /
```

Do the same for `armv7`. A verifier run that printed the "NOT checked" notice is
not ARM evidence by itself.

See `docs/rewrite/test-matrix.md` for the wider test matrix and
`docs/rewrite/layout-v1.md` for the installed layout.

## 4. Prove reproducibility

Build the same revision twice into separate directories and compare the files:

```sh
sh scripts/release/build.sh 1.0.0 build/release/repro-a
sh scripts/release/build.sh 1.0.0 build/release/repro-b
diff -rq build/release/repro-a build/release/repro-b
```

Any output from `diff` is a reproducibility failure; do not publish that
revision until it is explained. Record the checksums with the release evidence:

```sh
cat build/release/v1.0.0/checksums.txt
```

## 5. Create the GitHub Release manually

Commit the release state (including regenerated notices) and record the final
commit SHA. Then create the tag and the public release in one operator step,
pinning the tag to that verified commit:

```sh
# Manual. Never run from CI or from a release script.
VERIFIED_SHA=$(git rev-parse HEAD)   # the commit build.sh and verify.sh ran on
gh release create v1.0.0 \
  --target "$VERIFIED_SHA" \
  build/release/v1.0.0/rigscope_1.0.0_linux_amd64.tar.gz \
  build/release/v1.0.0/rigscope_1.0.0_linux_arm64.tar.gz \
  build/release/v1.0.0/rigscope_1.0.0_linux_armv7.tar.gz \
  build/release/v1.0.0/checksums.txt \
  build/release/v1.0.0/LICENSE \
  build/release/v1.0.0/rigscope_1.0.0_source.tar.gz \
  build/release/v1.0.0/install.sh \
  build/release/v1.0.0/THIRD_PARTY_NOTICES-go.md \
  build/release/v1.0.0/THIRD_PARTY_NOTICES.md \
  --title "Rigscope 1.0.0" \
  --generate-notes
```

Review the generated notes in `gh release view` afterwards; the release job does
not draft or edit them.

Without `--target`, `gh release create` would create a missing tag on the
repository's default branch, not on the verified commit. Verify the tag points
at the verified revision:

```sh
git fetch --tags
test "$(git rev-parse v1.0.0^{commit})" = "$VERIFIED_SHA" && echo "tag ok"
gh release view v1.0.0
```

Do not rebuild after tagging. `checksums.txt` is only meaningful for the exact
uploaded bytes, and `rigscope upgrade` verifies it against the staged binary's
reported version.

The manual "Release artifacts" workflow (`workflow_dispatch`, version input)
gives the same build and verification as steps 2–3 and keeps the assets as
workflow artifacts for inspection. It does not create the release.

## 6. Host `install.sh` on the website

`scripts/install.sh` is the single source of truth. The website build copies it
into the site output, so the published URL serves the tested script:

- implemented change: `website/scripts/build.mjs` copies
  `../scripts/install.sh` to `website/dist/install.sh`;
- deployed URL: `https://rigscope.dev/install.sh`;
- `scripts/install.sh` itself defaults to the GitHub Releases download root and
  honours `RIGSCOPE_VERSION` for a pinned install.

Build and inspect the site (does not deploy):

```sh
npm ci --prefix website
npm run typecheck --prefix website
npm run build --prefix website
head -n 3 website/dist/install.sh
```

Deployment is the operator's existing website deployment (the
`rigscope-website.service` host in `website/README.md`). The release tooling
must not start, restart or deploy that service. After deploying, confirm the
public URL and the quick start:

```sh
curl -fsSL https://rigscope.dev/install.sh | head -n 3
```

The quick-start command remains:

```sh
curl -fsSL https://rigscope.dev/install.sh | sh
```

## 7. Deprecate the 0.2 npm package (documentation only)

Do not run this during the release workflow and do not run it from CI. Once the
GitHub Release and website installer are live and tested, an operator may mark
the old npm line deprecated so `npm install rigscope` points users at 1.0:

```sh
npm deprecate 'rigscope@<=0.2.x' \
  'Rigscope 1.0 is a standalone Go binary. Install with: curl -fsSL https://rigscope.dev/install.sh | sh'
```

This does not unpublish 0.2.x, rewrite the package, or affect an existing
installation. There must be no `npm publish` step for 1.0: the npm package is
not a 1.0 delivery channel.

## 8. Upgrade path for the single 0.2 user

Rigscope 1.0 has no 0.2 data importer. The 0.2 state is not read, and 0.2
services must be gone before installing 1.0 because `rigscope install` refuses a
detected 0.2 installation.

1. Note the 0.2 version and keep any export or database copy outside the old
   installation tree for reference; it will not be imported:
   `npx --yes rigscope@0.2.6 status`, then copy the 0.2 state directory.
2. Stop and remove the 0.2 installation:
   `npx --yes rigscope@0.2.6 uninstall`. Do not use `--keep-data`: retained
   0.2 state is detected by the 1.0 installer and blocks installation.
3. Install 1.0 from the website installer:
   `curl -fsSL https://rigscope.dev/install.sh | sh`
4. Sign in as `owner` with the password chosen during installation. If you
   requested a generated password, the installer prints it once. A
   default `--role both` install has already paired the local collector with the
   local hub automatically; no grant is needed. Only a separate collector-only
   machine needs the hub's "Add machine" one-liner.
5. Confirm `rigscope version` prints the release version and both units are
   healthy (`rigscope status`, `rigscope doctor`).

From 1.0 on, `rigscope upgrade` performs the journalled activation described in
`docs/rewrite/layout-v1.md`; 1.x must upgrade in place without data loss.

Manifests that record the `adopted` state need a binary built from the
`adopted`-state commit or later; older binaries reject the unknown state.

Any user-scope `udev-gpu` manifest entry — `planned`, `owned` or `adopted` —
requires a binary from this branch or later. An older 1.0 binary does not know
the user-scope GPU rule layout or its `ruleGroup` field and rejects the manifest,
so a rollback to such a binary fails before it changes anything.

## Withdrawing a release

GitHub Releases and npm cannot be overwritten. If a release is wrong before it
has been installed anywhere, delete the GitHub Release and tag manually (an
operator action), keep the workflow artifacts for the record, and release a new
patch version. Never replace the uploaded assets under the same tag and never
`npm publish` a 1.0 package as a workaround.

## What remains manual

- Running the manual CI and release workflows.
- Creating and pushing the `v<version>` tag and the GitHub Release.
- Deploying the website build so `https://rigscope.dev/install.sh` is updated.
- Running `npm deprecate` for the old 0.2 line.
- The 0.2 uninstall on the one existing machine.
