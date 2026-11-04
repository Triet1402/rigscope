# System installation

System installation runs Rigscope under dedicated service accounts and starts
at boot without user lingering. For the default per-user npm setup, use the
[installation guide](installation.md).

Review the plan before applying changes. Commands that change the installation
require root and `--apply`; otherwise they only print the proposed changes.
The installed `rigscope system` command and `dist/system.cjs` use the same code.

| Task | Section |
| --- | --- |
| Review paths and permissions | [Generated layout](#scope-and-generated-layout) |
| Install or control services | [Component lifecycle](#system-component-lifecycle) |
| Enroll with another hub | [Connect to a hub](#connecting-to-an-independent-hub) |
| Grant package-energy access | [CPU package power](#cpu-package-power) |

### Scope and generated layout

```text
/opt/rigscope/                         root:root 0755
  releases/<version>/                  immutable root-owned release
  current -> releases/<version>
  previous -> releases/<previous>      retained for rollback
  node/node                            root-owned Node 26 when the selected binary was not root-owned
/etc/rigscope/
  hub.env                              root:root 0600 (credentials, never printed)
  collector.env                        root:root 0600
/var/lib/rigscope/
  hub/telemetry.sqlite                 rigscope-hub 0600, private 0700 directory
  hub/enrollment.json                  rigscope-hub 0600
  hub/run/hub.sock                     private Unix socket (remote mode)
  collector/state.json                 rigscope-collector 0600
  collector/outbox/                    durable collector outboxes
  backups/                             root:root 0700, pre-upgrade/pre-rollback SQLite copies
  system-install.json                  root:root 0600, version 2 component state/versions/backups/adoption
  power-access.json                    root:root 0600, original counter modes for reversal
/etc/systemd/system/rigscope-hub.service       root:root 0644, hub boot unit
/etc/systemd/system/rigscope-collector.service root:root 0644, collector boot unit
/etc/sysusers.d/rigscope.conf          root:root 0644, both nologin accounts
/etc/sysusers.d/rigscope-rapl.conf     root:root 0644, collector-only power membership
/etc/udev/rules.d/90-rigscope-rapl.rules root:root 0644
```

Build the checkout before using `dist/system.cjs`; an installed package exposes the same commands through `rigscope system`. Reference copies are in `packaging/system/`. They are for review only; the
renderer in `src/cli/system.ts` is authoritative.

`systemd-sysusers` creates `rigscope-hub` and `rigscope-collector` as nologin
system accounts even when one component is disabled, so the other can be
enabled later without reinstalling. The hub and collector never run as root.
The units set `NoNewPrivileges=yes`, `ProtectSystem=strict`,
`ProtectHome=yes`, an empty `CapabilityBoundingSet=`/`AmbientCapabilities=`,
`UMask=0077`, explicit writable state paths and a read-only release tree. The
collector uses `--await-connection` to exit cleanly when no destinations are
configured, including after an interrupted enrollment. The
collector does **not** use `PrivateDevices=yes` because GPU telemetry still
needs real device nodes. The hub does.

### System component lifecycle

All mutating commands require `--apply` and root. Without `--apply`, the exact
files and command sequence are printed and nothing is written. Every component
operation touches only its own unit; enabling or starting one component never
stops or restarts the other.

```sh
# Package first (from a built checkout).
node scripts/package-release.cjs --output /var/tmp/rigscope-<version>.tar.gz

# Review a release before granting sudo.
node dist/system.cjs plan \
  --source /var/tmp/rigscope-<version>.tar.gz --role local --node /usr/bin/node

# Install one complete package. --role selects the initial enabled preset.
sudo node dist/system.cjs install \
  --source /var/tmp/rigscope-<version>.tar.gz --role hub \
  --node /usr/bin/node --install-deps --enable --apply

# Add the other component later; no reinstall and no new package are needed.
sudo node dist/system.cjs enable collector --apply
sudo node dist/system.cjs enable hub --apply

# Component-specific lifecycle when both exist.
sudo node dist/system.cjs disable collector --apply
sudo node dist/system.cjs restart hub --apply
sudo node dist/system.cjs restart collector --apply
sudo node dist/system.cjs restart --apply   # enabled components only

# Component-specific status and logs.
node dist/system.cjs status
node dist/system.cjs status collector --json
node dist/system.cjs logs hub --follow
```

`install` stages a complete release, verifies both runtimes and UI artifacts,
copies a non-root-owned Node executable into the root-owned prefix, installs
production dependencies when `--install-deps` is given, writes private
configuration and both units, creates both accounts, then switches `current`.
Staging happens before any service is stopped.

- With `--enable`, `--role local` initially enables both components.
- With `--enable`, `--role hub` initially enables only the hub.
- With `--enable`, `--role collector` initially enables only the collector.
- Without `--enable`, both components are installed but disabled.
- `--start` starts only the requested preset and verifies those components.
- `--without-power` skips the collector RAPL grant at that mutation.

`enable hub` creates the owner account once when `HUB_BROWSER_USERS` is empty
and prints the password only in that mutation. It is never stored in plaintext
or printed again. `enable collector` completes the collector account, unit,
identity and RAPL grant. With no destination it tries the colocated hub:

- A live configured hub gets a short-lived, single-use grant in its
  authoritative SQLite registry. The collector then enrolls through the normal
  hub API, which updates the live registry without stopping or restarting the
  hub.
- A stopped hub gets the identity written to the authoritative registry (or a
  static `HUB_COLLECTORS` bootstrap when the registry is not initialized yet);
  enabling the collector never starts the hub.
- If there is no configured hub, the collector is enabled but stays dormant and
  reports `Awaiting connection`. Its unit uses `--await-connection` to exit
  cleanly before collection until a destination is saved.

Configured remote destinations and credentials are never overwritten. Re-running
`enable collector` leaves its destination, token, identity and outboxes intact.

### Connecting to an independent hub

For a hub-first test where the hub and collector are separate installations,
create a connection grant in the hub's admin Collectors panel. Save the grant to
a private file and connect the system collector:

```sh
# On the collector host; the grant file must be root-owned and 0600.
sudo install -m 0600 /dev/null /root/collector-grant
sudoedit /root/collector-grant        # paste the single-use grant, save, quit
sudo node dist/system.cjs connect https://<hub-host> \
  --token-file /root/collector-grant --apply
sudo rm /root/collector-grant
```

`connect` stops only the collector while it enrolls, calls the hub's normal
enrollment API, persists the issued credential under
`/var/lib/rigscope/collector/state.json`, and starts the collector if it is
enabled. The grant is never printed. Re-running `connect` on a configured
collector is refused rather than replacing a remote destination.

Status reports each component independently:

```text
Components:
  hub: installed, enabled, active, ready
  collector: installed, enabled, active, ready, 1 destination(s)
```

`readiness` is `ready`, `stopped`, `unconfigured`, `awaiting-connection`,
`invalid-config`, `invalid-state` or `missing`.

### CPU package power

`collector/sampling/hwmon.ts` already reads top-level RAPL package counters,
sums multiple sockets, handles wrap/reset and returns unavailable rather than
zero. On hosts where `energy_uj` is root-only, system scope grants a narrow
read path instead of elevating Node.

```sh
node dist/system.cjs power status
sudo node dist/system.cjs power grant --apply
sudo node dist/system.cjs power revoke --apply
```

Mechanism:

- `/etc/sysusers.d/rigscope-rapl.conf` creates `rigscope-rapl` and adds only
  `rigscope-collector`; the hub is not a member.
- A `powercap` udev rule matches only top-level `intel-rapl:N` devices named
  `package*` and changes only `energy_uj` and `max_energy_range_uj` to
  `0440 root:rigscope-rapl`. Subdomains, constraint files and power-limit
  writes are untouched.
- `udevadm trigger --subsystem-match=powercap --action=change` applies the rule
  immediately. Because the rule lives in `/etc/udev/rules.d`, device
  recreation and boot reapply it.
- The collector unit adds `SupplementaryGroups=rigscope-rapl`; it does not use
  file capabilities, ambient capabilities or a privileged Node helper.
- Grant records each existing counter's original mode/uid/gid in
  `/var/lib/rigscope/power-access.json`. Revoke removes the rule and group
  membership and restores those recorded values (defaults `0400` for
  `energy_uj` and `0444` for `max_energy_range_uj` when no record exists).
- `power status` reports driver modules, package domains, collector
  readability, persistence, world readability and every problem. A missing
  counter is reported unavailable, never as zero watts.

Verify real watts in History after granting access and restarting the collector;
do not treat a mocked value or a one-time manual `chmod` as installation
evidence. Unsupported hardware remains unavailable.
