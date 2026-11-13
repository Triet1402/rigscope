# Installer lifecycle end-to-end test

`lifecycle.sh` builds two real releases with `scripts/release/build.sh`, serves
them from a local HTTPS server with a dedicated test CA, and drives the whole
1.0 lifecycle with the installed binaries:

1. `install.sh --role both --boot` from the served assets, into a disposable
   `HOME` and `XDG_*` tree.
2. Read the owner password from the fake hub journal, log in, create a
   collector grant through the admin API, enroll the local collector and
   confirm its machine has fresh metrics in `/api/live` and points in
   `/api/history`.
3. Verify the fresh hub defaults to RAM history, then explicitly enable
   persistent telemetry and restart the disposable hub for the history
   preservation checks.
4. Install a second collector in another disposable `HOME`, pointed at the
   first hub, and confirm both machines report.
5. `status --json`, `doctor`, `upgrade --version 1.0.0-e2e.2`, `rollback`,
   `uninstall`, and `uninstall --keep-data` on a fresh install. The assertions
   cover service restarts, the upgrade journal phases, preserved machines and
   continuous history, and that uninstall removes only owned paths.

Run it from the repository root:

```sh
sh scripts/e2e/lifecycle.sh
```

Set `RIGSCOPE_E2E_KEEP=1` to keep the disposable directory for debugging. The
script prints that directory at the start and dumps fake journals and command
logs when a step fails.

## Safety

The test never reaches the real systemd, journald, logind, `/etc` or `/usr`.
Before any product command it builds fake `systemctl`, `journalctl`, `loginctl`,
`systemd-sysusers` and `udevadm` binaries (`fake-systemd`), puts their directory
first on `PATH`, and refuses to continue unless `command -v systemctl` resolves
to the fake and the child `HOME` is below the disposable directory. The fake
`systemctl` only reads unit files and keeps pids, logs and enablement markers
under `RIGSCOPE_FAKE_SYSTEMD_ROOT`; it launches `ExecStart` as a supervised
background process and implements the `show -p LoadState,ActiveState,SubState
--value` contract the manager reads.

## Loopback release override

`internal/release` has one test-only seam for the upgrade path:

```sh
RIGSCOPE_RELEASE_BASE_URL=https://127.0.0.1:8443/releases
```

It replaces the default GitHub Releases API root only when the value is an
HTTPS URL on `localhost` or a loopback IP, it applies only to callers that did
not select an explicit `APIBase`, and it does not relax the HTTPS or redirect
policy. Production defaults are unchanged. `install.sh` uses its existing
`RIGSCOPE_INSTALL_BASE_URL` override for the asset download root.
