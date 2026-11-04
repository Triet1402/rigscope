# Installation

## Install a release

Linux, Node.js 26+ with npm and a working systemd user manager are required for
background installation. Install Node.js before running Rigscope; the user installer
uses the selected Node executable. No global npm installation or source checkout is needed:

```sh
npx --yes rigscope@latest install --boot
~/.local/bin/rigscope
```

If the npm release is not available yet, use [Install from an archive](#install-from-an-archive).

Save the generated `owner` password. Open the printed dashboard URL. The installer
starts a verified background service and returns to the shell. Closing the terminal
does not stop it. The service restarts after crashes. `--boot` runs
`loginctl enable-linger` for the current user so startup does not require a login;
if host authorization prevents it, the installer reports the exact recovery command.
Omit `--boot` to start at login only. `status` reports the actual startup setting.

`--no-start` leaves a new installation stopped, and `--no-enable` disables login
startup. Repeating `install` repairs registration/launcher and startup settings
without replacing the runtime or credentials; use `rigscope upgrade` to change code.
Re-registration with `--no-start` leaves an already-running service running.

| Goal | Install options | Runs |
| --- | --- | --- |
| Monitor this computer | `install --boot` | Background hub and local collector |
| Connect to a hub | `install --role collector --boot` | Collector after enrollment |
| Host and monitor through HTTPS | `install --origin https://hub.example.com --boot` | Background hub and colocated collector |
| Dedicated remote dashboard | `install --role hub --origin https://hub.example.com --boot` | Hub without collection |
| Foreground evaluation | `try` | Isolated hub and collector on port 8082 |

Add `~/.local/bin` to PATH if necessary. The generated command is a small launcher
that pins the installation's Node executable, state directory and stable managed
runtime path. It survives deletion of an npx cache or the original source checkout.
The service uses the same Node and runtime. An unrelated command at the destination
is preserved and the installer prints the usable absolute command path instead.
An owned symlink into the checkout used for installation is replaced by the managed
launcher. `install --from /path/release.tgz` can also stage a supplied archive.

### Install from an archive

For testing before npm publication, or for a downloaded release, run from the
directory containing the archive or supply its absolute path:

```sh
npx --yes --package ./rigscope-0.1.0.tgz rigscope install --boot
```

The archive contains the built application. npm still needs access to its registry
or a populated cache to install production dependencies. A `.tgz` alone is not a
complete offline installation. All role, origin and startup options below also
work with the archive command.

## Everyday commands and installation selection

Bare `rigscope` shows status and never initializes state or starts a trial.
`start` is idempotent. `status` includes scope, role, startup, PID, CLI/runtime
versions, and live hub health/version when a user service is active. `logs` shows
100 journal lines; `logs --follow` follows new entries.

```sh
rigscope
rigscope start
rigscope stop
rigscope logs --follow
rigscope upgrade
rigscope uninstall
```

`uninstall` removes both user services, their matching launcher and installed runtime
copies, preserving accounts, history, identity, queues and backups. Reinstall
using npm or an archive to restore the application with retained settings. No data purge is implicit.

## Access from another computer

For a new installation, set the browser-visible HTTPS origin at install time:

```sh
npx --yes rigscope@latest install \
  --origin https://machine.example.ts.net:8081 --boot
```

The default `local` role means a colocated hub and collector. It supports either
localhost or HTTPS access. Its collector uses the configured HTTPS endpoint, with
normal certificate validation and its existing machine credential. No second
collector or grant is needed. Use `--role hub` for an initial dashboard without
collection, then `rigscope enable collector` to add collection later.
`enable` and `disable` control each component independently; `start`, `stop`,
`restart` and `logs` accept `hub` or `collector`.
For a hub-first setup, install `--role hub`, then enroll a separate collector using **Add machine**.

Run the HTTPS proxy on the hub machine. With Tailscale already configured, preserve
other Serve routes and use TLS-terminated TCP forwarding to preserve Rigscope's
exact Host/Origin boundary:

```sh
tailscale serve --bg --tls-terminated-tcp=8081 \
  "unix:$HOME/.local/state/rigscope/run/hub.sock"
```

Use the actual socket printed by the installer if selecting another state directory.
Open the configured HTTPS origin from a computer permitted to reach the hub through
Tailscale. The viewing computer needs no Rigscope installation unless it should
also be monitored. The background proxy and Rigscope service continue after closing
the terminal.

### Convert a retained localhost installation

`uninstall` removes services, the command and runtime copies, while preserving
configuration, accounts, history, identity and queues. A reinstall with different role or
origin settings therefore refuses rather than silently resetting that state.
Change access explicitly instead:

```sh
rigscope stop
rigscope configure --origin https://machine.example.ts.net:8081
rigscope start
```

`configure` requires a stopped hub and also a stopped collector when its colocated
destination changes. It validates the new origin/socket, backs up the
previous private settings, and updates the colocated collector's matching destination.
It moves that destination's entire outbox, including SQLite WAL, without changing
credentials or other hubs. An existing target outbox refuses conversion to avoid
merging or discarding pending records. Accounts and the history database are untouched.
Run `configure --local` while stopped to return to localhost. Rollback targets must declare support for the saved configuration and database schema.

If already uninstalled, configure retained state through the package entry point,
then reinstall the full application:

```sh
npx --yes rigscope@latest configure \
  --origin https://machine.example.ts.net:8081
npx --yes rigscope@latest install --boot
```

Before npm publication, substitute the archive entry point shown above.
Users, history and identity remain intact when restoring the launcher/services.

## Add machines and hubs

1. Install the collector role on the new machine. It stays stopped until enrolled.
2. On the hub choose **Add machine**, then **New machine** and create a connection
   grant. Grants are single-use and expire within 15 minutes.
3. Run the displayed `rigscope connect https://hub.example.com` command on the new
   machine. Paste the grant at its private prompt.
4. The installed collector starts in the background after successful enrollment.
   Use `--no-start` to enroll without starting.

On the collector machine:

```sh
npx --yes rigscope@latest install --role collector --boot
~/.local/bin/rigscope connect https://hub.example.com
```

Stop a running collector before changing destinations. Replacement uses a grant
explicitly bound to the existing machine and `connect --replace`.
For unattended enrollment, put the grant in an owned private regular file and use
`--token-file /private/path`. It is never accepted as a command-line token. Delete
the file after use. Generated commands contain no grants, and clipboard contents
remain under the user's control.

Collectors connect outbound: no incoming collector port and no root collector are
needed. Each collector samples once and reports independently to up to eight hubs.
Retry queues persist across restart. CPU, memory, swap, GPU, disk, network rates and
on-demand processes are shared by default. Network addresses and full process
arguments are disabled. Per-hub sharing settings are in `collector/state.json`;
edit while stopped. Existing destinations keep their policies.

A hub has no fixed machine-count limit. Its concurrent connection budget defaults
to 100, configurable with `maxCollectorConnections` in private user `config.json`
or `HUB_MAX_COLLECTOR_CONNECTIONS` for raw/system hub services. Restart after edits.

## Explicit foreground trial

```sh
rigscope try
# Ctrl+C stops the trial hub and collector.
```

The default trial uses a separate `~/.local/state/rigscope-trial` directory and
port 8082. Trial credentials/history persist. `try --state-dir PATH --port PORT`
selects another trial. A managed runtime directory cannot be used for `try`.
`start --foreground` intentionally runs an existing installation in the terminal;
stop its background service first. It does not install a systemd service.

From source, prepare an archive with `npm ci` then `npm run package:npm`.
End users run the built package; they do not need a source build or checkout deployment command.

## Hub configuration

`rigscope config` prints the owned 0600 `config.json` path. `--state-dir PATH` or
`RIGSCOPE_STATE_DIR` selects another installation; directories must be private (0700).
Each installation has separate hashed hub and collector service names. The installed alias defaults to
the state directory it belongs to. Settings include the initial role preset, independent component enablement, local port, remote origin,
socket, bootstrap users and local collector mapping. Restart after edits. Managed hub
users become authoritative in SQLite after the first start; editing bootstrap password
hashes later does not change existing accounts. Use Administration for user changes.

A new hub prints an `owner` password once and stores only a salted scrypt hash. Save
that password. Local trial mode always listens on loopback and still requires login.
The trial persists real samples and identity; restarting it reuses the same credentials.

For a remote hub, forward the exact configured HTTPS origin to its private Unix socket,
for example with Tailscale Serve or a TLS reverse proxy running as the same user.
Configure the proxy separately and keep the socket private. The hub opens no plaintext
TCP listener in this mode; remote collectors use normal certificate-validated WSS.
Do not turn off TLS verification or expose the local trial listener to a LAN.
See the [HTTPS boundary](development.md#https-boundary) for proxy requirements.

## Upgrades

Use the installed command to upgrade the managed runtime and preserve its data:

```sh
rigscope upgrade                              # latest published npm release
rigscope upgrade --from rigscope@0.1.0         # select an exact published version
rigscope upgrade --from ./rigscope-VERSION.tgz # select a local archive
rigscope rollback                             # return to the retained previous runtime
```

Run only the command for the desired operation. A fresh installation has no
previous runtime to roll back to. Updating a global npm package does not update
the runtime managed by `rigscope install`.

`rigscope doctor` checks the installation's runtime, private configuration,
service, database, listener, TLS destinations and sensor access. `--json` produces
redacted structured output. Checks follow component enablement: a hub does not need
an enabled collector.

`rigscope upgrade --from ./rigscope-VERSION.tgz` stages while active components
run, then stops them, verifies runtime leases and preserves a consistent database
backup paired with the old runtime. After activation it checks the running hub's
embedded version, database schema/integrity and, for active collectors, a fresh
sampling marker from the collector process. `--timeout SECONDS` bounds activation
health verification. A failed activation stops the attempted runtime before restoring
the previous code and, when needed, its matching database copy. Recovery is verified
before it is called healthy; an unresolved recovery error is reported explicitly.
Only components running before the upgrade are restarted. Startup settings are
preserved independently. Inactive components stay inactive.

`rigscope rollback` uses `runtime-previous` and `release-state.json`. Compatible
current databases retain post-upgrade data. An incompatible schema needs the paired
pre-upgrade backup (or an explicit compatible `--backup PATH`); the replaced database
is backed up first. Missing pairings fail closed. Do not manually move runtimes or
run old code against newer schemas. Keep separate off-machine backups.

For a lost owner password, stop the installation and use
`rigscope recover-password --user owner`. Entry is private and confirmed twice;
noninteractive use requires `--password-file` naming an owned private regular file.
Recovery changes the durable managed user, records an audit event and invalidation
marker, and leaves bootstrap configuration, machine access and collector credentials
intact. It refuses a live database lease and does not create or elevate users.
The stopped hub's in-memory sessions are gone; restart and use the new password.

Without `--from`, upgrade requests `rigscope@latest`. Before the first npm
publication, use `--from` with a local archive.

## Hardware

Linux is the supported collection platform. CPU/memory/process counters use procfs,
not CPU-vendor-specific tools. ARM/RISC-V CPU naming recognizes their common fields;
CPU utilization continues to use per-core procfs counters.

| Hardware | Available readings | Limit |
| --- | --- | --- |
| NVIDIA | Utilization, dedicated memory, temperature, power; compute-process memory | Requires `nvidia-smi`; existing asynchronous bounded polling |
| AMD discrete GPU | Busy percentage, dedicated VRAM, temperature, power | Requires readable amdgpu sysfs/hwmon counters |
| AMD integrated GPU | Busy percentage and available hardware sensors | Driver-reported VRAM only; no synthesized shared-memory totals |
| Intel discrete or integrated GPU | Available hwmon temperature/power; optional driver-exposed busy/VRAM counters | No general Intel utilization/engine backend yet; most Intel utilization remains unavailable |
| ASPEED/other display adapters | Inventory | No telemetry backend |

On AMD APUs the reported power counter can include CPU/SoC power; it is not a
GPU-exclusive power measurement.

Every field is optional. Drivers, permissions and kernel versions determine what can
be read. An adapter with no readable counters remains unsupported; missing readings
never become successful zeros. Intel sensor presence does not imply utilization or
per-process GPU accounting support. Hotplug uses the existing inventory refresh.
AMD/Intel adapters retain PCI-based IDs, while NVIDIA retains its UUID-based IDs.

AMD uses the kernel's [amdgpu sysfs and hwmon interfaces](https://docs.kernel.org/gpu/amdgpu/thermal.html).
The Radeon 860M iGPU and RTX 5060 Laptop GPU have now been checked on the actual
laptop, including fresh iGPU utilization, driver-reported memory, temperature and
power samples persisted by the hub. The Ubuntu RTX 3090 rig remains validated. Intel
sensor and ARM/RISC-V naming paths have fixture coverage and still need physical
validation. macOS, Windows, Apple GPU telemetry and a
complete Intel GPU utilization backend are not included in this release pass.


## System installation

The optional system scope uses dedicated unprivileged accounts and starts at boot without user lingering. Review its plan before applying changes. Mutations require root and `--apply`; without `--apply` they print the proposed changes. The installed `rigscope system` command uses the same implementation as `dist/system.cjs`.

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
