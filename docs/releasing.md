# Releasing Rigscope

The first public version, `0.1.0`, is published on
[npm](https://www.npmjs.com/package/rigscope). Source and issues are on
[GitHub](https://github.com/Triet1402/rigscope). The package includes MIT licensing,
dependency notices, the security policy and [release notes](../CHANGELOG.md).

The commands below use `0.1.0` as an example. For a new release, substitute its
new version everywhere; do not attempt to republish `0.1.0`. Documentation edits
can be committed and pushed to GitHub independently. The published archive and
its bundled documentation stay unchanged; ship updated package documentation
with the next version. Keep each release tag on the revision used to build it.

## Recommended release order

1. Update the version, docs, release notes and repository metadata. Commit them.
2. Push the source to GitHub and run the manual CI workflow on that revision.
   CI runs manually; pushes and pull requests do not trigger it.
3. Build one npm archive from the final revision. Retain it, its SHA-256 checksum
   and verification results. Test and publish that archive without rebuilding it.
4. Run the publish dry run, check the npm account, and publish the verified archive.
5. Verify the registry version and install it into a disposable test installation.
   Check sign-in, fresh telemetry and the installed command.
6. Tag the exact source revision as `v0.1.0`, push the tag, and create a GitHub
   Release with the release notes, verified archive and checksum. Announce it
   after the registry installation works.

For subsequent releases, update the package and lockfile versions together
(for example, `npm version patch --no-git-tag-version`), update the release notes,
and repeat the same sequence. A published name/version cannot be overwritten,
so a correction after publication needs a new version. Prereleases should use
an explicit tag such as `next`; ordinary `rigscope upgrade` follows `latest`.

## Build the final archive

Run from the source checkout with Linux and Node.js 26+:

```sh
npm ci
npm run typecheck
mkdir -p out/release
npm pack --pack-destination out/release
```

`npm pack` runs `prepack`, which regenerates dependency notices, builds fresh
runtime/UI artifacts and verifies them. If notice generation changes a tracked
file, include that change in the release commit before producing the final
archive. Reuse the archive for every check below. Replace `0.1.0` in these
commands when releasing another version.

## Verify the exact archive

```sh
node scripts/verify-package.cjs ./out/release/rigscope-0.1.0.tgz
npm run verify:installed -- --archive ./out/release/rigscope-0.1.0.tgz
npm test
node scripts/test-runtime.cjs
node scripts/test.cjs --suite development
```

Package inspection verifies the built CLI/runtime/UI and excludes source tests,
Git data, private environment files and caches. Inspect the file list for
unexpected state, databases, credentials or logs. Installed-package verification
uses production dependencies in a temporary directory, audits the resolved
dependency tree and checks the CLI version plus authenticated sign-in/History.

The following opt-in checks require a working Linux systemd user manager. They
create disposable installations with separate state, launchers, ports and unit
names, then remove their services. They do not select an existing installation.

```sh
RIGSCOPE_TEST_ARCHIVE="$PWD/out/release/rigscope-0.1.0.tgz" \
  node --import tsx --test tests/cli-install.integration.ts
node scripts/verify-operations.cjs ./out/release/rigscope-0.1.0.tgz
```

These cover installation, pairing, independent component controls, bootstrap
removal, retained-data reinstall, upgrade/rollback, failed-start recovery and
offline password recovery. Runtime tests additionally cover authenticated HTTPS,
SSE, collector connections, persistence and reconnect behavior in isolated setups.

On the reference host with 48 logical cores, two RTX 3090s and an ASPEED adapter,
run the hardware-specific test explicitly:

```sh
RIGSCOPE_REAL_HW=1 node --import tsx --test tests/sampling-real.test.ts
```

That test reads the actual host and is skipped by default. Its exact inventory
assertions are specific to that rig; it is not a portable hardware test.

Record the final checksum after verification:

```sh
cd out/release
sha256sum rigscope-0.1.0.tgz > SHA256SUMS
sha256sum --check SHA256SUMS
```

Keep the source revision, archive checksum, check results and any skips together.
Record boot enablement separately from an actual reboot. Physical laptop testing
and synthetic scale workloads are separate evidence; see
[hardware support](installation.md#hardware) and [scale validation](scale-validation.md).

## Publish dry run and publication

Run these commands from the source checkout. Sign in interactively if needed,
then verify the account and preview the exact archive:

```sh
npm login --registry=https://registry.npmjs.org
npm whoami --registry=https://registry.npmjs.org
npm publish ./out/release/rigscope-0.1.0.tgz --access public --tag latest \
  --registry=https://registry.npmjs.org --dry-run
```

The dry run checks the package without publishing. It does not verify permission
to publish or complete two-factor authentication.

When releasing, this command actually publishes the verified archive publicly:

```sh
npm publish ./out/release/rigscope-0.1.0.tgz --access public --tag latest \
  --registry=https://registry.npmjs.org
npm view rigscope@0.1.0 version dist.integrity --registry=https://registry.npmjs.org
```

Then use `npx --yes rigscope@0.1.0 install` with a disposable `--state-dir`, an
unused `--port` and an isolated `XDG_BIN_HOME` for the registry installation
check. Uninstall that test installation afterward. Use the same isolation
settings for all test commands so the check does not select a real installation.

The public quick start uses `npx --yes rigscope@latest install --boot` and the
installed `rigscope upgrade`. Archive commands remain available for downloaded
releases and testing before publication. Users do not need a global npm
installation of Rigscope.

GitHub Actions trusted publishing can automate later releases. Configure it for
the actual repository and publishing workflow; the current CI workflow only
verifies and does not publish. See npm's
[publishing documentation](https://docs.npmjs.com/cli/commands/npm-publish/) and
[trusted publishing guide](https://docs.npmjs.com/trusted-publishers/).

## Alternative manifest archives

`npm run package:release` creates an operations `.tar.gz` with a hashed manifest.
It is a separate format from the npm `.tgz`. The low-level installer validates
size/hash and required artifacts before activation. Production dependencies must
be installed in the staged release; activation never silently runs npm or service
commands. See [operations](operations.md#low-level-manifest-archive-workflow) and
[system installation](system-installation.md).

Configuration, database schema and wire-protocol revisions are internal
compatibility checks, independent of the application package version.
