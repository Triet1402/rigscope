# Installation

Start with [npm installation](#install-from-npm). For a shared dashboard, see
[remote access](#access-from-another-computer) and [adding machines](#add-machines-and-hubs).
[Upgrades](#upgrades) and [troubleshooting](#troubleshooting) cover existing installs.

## Install from npm

Requires Linux, Node.js 26+ with npm, and a working systemd user manager. Install
Node.js first; Rigscope uses that Node executable for its background services.

```sh
npx --yes rigscope@latest install --boot
```

Save the generated `owner` password and open the printed URL, normally
`http://127.0.0.1:8081`. The password is shown only when the account is first
created. An existing installation keeps its account and password.

The installer starts a hub and a collector in the background. Closing the
terminal does not stop them. `--boot` enables startup at boot, including before
login. If the host requires authorization, the installer prints the recovery
command. Omit `--boot` to start at login instead.

Add `~/.local/bin` to PATH, or run `~/.local/bin/rigscope` directly. No global npm
install or source checkout is needed. The launcher and services use the installed
runtime, so removing the npx cache does not break them.

Releases are on [npm](https://www.npmjs.com/package/rigscope). `@latest` selects
the current stable release; replace it with `@0.1.4` to select that exact version.

### Installation options

Append these options to `npx --yes rigscope@latest install`:

| Goal | Options |
| --- | --- |
| Monitor this computer | `--boot` |
| Send readings to another hub | `--role collector --boot` |
| Host a dashboard over HTTPS and monitor this computer | `--origin https://hub.example.com --boot` |
| Host a dashboard without collecting local readings | `--role hub --origin https://hub.example.com --boot` |
| Use another local port | `--port 8086 --boot` |
| Leave a new installation stopped | `--no-start` |
| Disable startup at login | `--no-enable` |

Repeating `install` repairs the services, launcher and startup settings. It keeps
the existing runtime and credentials; use `rigscope upgrade` to update the code.
`--no-start` does not stop a service that is already running.

### Install from an archive

For a downloaded package or one built from source, run from the directory
containing the archive, or give its absolute path:

```sh
npx --yes --package ./rigscope-0.1.4.tgz rigscope install --boot
```

The same installation options apply. npm still needs registry access or cached
production dependencies; the archive alone is not a complete offline installer.

## Everyday commands and installation selection

Run only the command for the task you want:

| Task | Command |
| --- | --- |
| Check services, versions and the dashboard URL | `rigscope status` (or just `rigscope`) |
| Start or stop | `rigscope start` / `rigscope stop` |
| Restart the hub | `rigscope restart hub` |
| Stop local collection | `rigscope stop collector` |
| Follow logs | `rigscope logs --follow` |
| Check configuration, services and sensor access | `rigscope doctor` |
| Remove the installation and all its data | `rigscope uninstall` |
| Remove services and runtime, keeping saved data | `rigscope uninstall --keep-data` |

`start` is safe to repeat. `logs` shows the last 100 journal lines by default.
`enable` and `disable` control the hub and collector independently; the initial
role is a preset. Use `rigscope enable collector` to add local collection to a
hub-only installation.

`uninstall` permanently deletes the selected user installation, including
accounts, history, collector credentials, pending samples and backups. It stops
owned services and foreground runtimes and never restarts them. `--purge` remains
an explicit alias for this default.

To keep saved data for a later reinstall, opt in:

```sh
rigscope uninstall --keep-data
```

If the launcher is already gone, use the package entry point:

```sh
npx --yes rigscope@latest uninstall
npx --yes rigscope@latest install --role collector --boot
```

Pass `--state-dir PATH` to select a separate installation. Reinstalling after
`uninstall --keep-data` reuses saved settings and the existing password. This
retention applies to canonical installations; upgrading a pre-canonical format
resets configuration and history, so back up the state directory first.

### Changes while running

Commands that need exclusive access detect the selected installation's running
services and foreground runtimes and ask before stopping them. After a successful
change, previously running components resume automatically. Uninstall never
restarts anything.

Use `--yes` to approve stopping without a prompt, and `--no-restart` to leave the
installation stopped after a change. In scripts, pass these flags to Rigscope:

```sh
npx --yes rigscope@latest configure --port 8086 --yes --no-restart
```

The first `--yes` belongs to npm; the second approves Rigscope's stop prompt.
`--no-start` also prevents startup on commands that support it. Components that
were already stopped stay stopped when restoring a running installation. Foreign
or unidentifiable processes are never stopped automatically. Foreground trials
resume as background processes when there is no installed service; their logs
are written under the selected state directory in `run/hub.log` and
`run/collector.log`.

Terminal output uses color for headings, status and errors. Piped output and
JSON remain plain; set `NO_COLOR=1` to disable color in a terminal.

## Access from another computer

Run the commands on the hub machine. The examples use Tailscale already signed
in, with HTTPS enabled. Replace `machine.example.ts.net` with the hub's Tailscale
DNS name. Choose the section for a new or existing installation.

### New HTTPS installation

```sh
npx --yes rigscope@latest install \
  --origin https://machine.example.ts.net:8081 --boot
tailscale serve --bg --tls-terminated-tcp=8081 \
  "unix:$HOME/.local/state/rigscope/run/hub.sock"
```

### Convert a retained localhost installation

If you already installed for localhost, change access without reinstalling:

```sh
rigscope configure --origin https://machine.example.ts.net:8081
tailscale serve --bg --tls-terminated-tcp=8081 \
  "unix:$HOME/.local/state/rigscope/run/hub.sock"
```

This keeps accounts, history and collector credentials. It updates the local
collector's destination and moves its pending samples. Rigscope asks to stop
running components during conversion and resumes them afterward. See [Access changes](operations.md#access-changes)
for backup and queue checks.

### Open and check the dashboard

Open `https://machine.example.ts.net:8081/` from a device allowed to reach the
hub through Tailscale. Sign in with the existing account or the password printed
at first installation. The viewing device needs no Rigscope installation.

```sh
rigscope status
tailscale serve status
```

The forwarding command changes only port 8081 and keeps other Serve routes.
Use the socket path printed by Rigscope if you chose another state directory.
`--bg` keeps the route active after the terminal closes and across reboots.

Use TLS-terminated TCP forwarding to preserve the request's Host header,
including its port. See the [Tailscale Serve reference](https://tailscale.com/docs/reference/tailscale-cli/serve).
Remote mode listens on a private Unix socket instead of a plaintext TCP port;
collectors connect through HTTPS with certificate validation.

### Return to localhost

```sh
rigscope configure --local
tailscale serve --bg --tls-terminated-tcp=8081 off
```

If you used `uninstall --keep-data`, change retained settings through the package entry
point, then reinstall:

```sh
npx --yes rigscope@latest configure \
  --origin https://machine.example.ts.net:8081
npx --yes rigscope@latest install --boot
```

For a local port, use `configure --local --port 8086` instead. `configure` cannot
combine `--origin` and `--port`, because HTTPS mode uses a private Unix socket
instead of the port.

## Add machines and hubs

1. On the hub, choose **Add machine**, then **New machine**. Create a connection
   grant; it can be used once and expires within 15 minutes.
2. On the machine to monitor, run:

   ```sh
   npx --yes rigscope@latest install --role collector --boot
   ~/.local/bin/rigscope connect https://hub.example.com
   ```

3. Paste the grant at the private prompt. The collector starts after enrollment.
   Use `connect --no-start` to enroll without starting it.

Collectors connect outbound and need no incoming port or root access. Each can
send to up to eight hubs. Sharing is separate for each hub; network addresses
and full process arguments start disabled. See [Collection and sharing](monitoring.md#collection-and-sharing).

Changing destinations asks to stop a running collector and resumes it afterward. To replace an
existing machine credential, create a grant bound to that machine and use
`connect --replace`. For unattended enrollment, use `--token-file` with an owned
private file and delete it afterward; do not put the grant on the command line.

## Explicit foreground trial

```sh
npx --yes rigscope@latest try
```

Ctrl+C stops the trial. It uses separate data at `~/.local/state/rigscope-trial`
and port 8082. Trial accounts and history persist. Use `try --state-dir PATH
--port PORT` to choose another trial directory and port.

To run an existing installation in the terminal, stop its background service
first, then use `rigscope start --foreground`. This does not install services.
A managed runtime directory cannot be used for `try`.

## Hub configuration

`rigscope config` prints the private `config.json` path. The installed launcher
selects its own state directory. `--state-dir PATH` or `RIGSCOPE_STATE_DIR`
selects another installation, with separate data and service names. The file has
one canonical shape: it has no version field, and its required `components` object
records hub and collector enablement.

Settings include component enablement, port, HTTPS origin and socket. Keep state
directories at mode `0700` and configuration files at `0600`. Restart after edits.
Use `configure` for access changes and **Administration** for users: editing
first-run password hashes does not change an account already stored in SQLite.

There is no fixed machine-count limit. The default connection limit is 100.
Set `maxCollectorConnections` in private `config.json` and restart to change it.
For direct hub/collector services, see the [environment reference](configuration.md).
For another HTTPS proxy, see the [proxy requirements](development.md#https-boundary).

## Upgrades

Use the installed command:

```sh
rigscope upgrade
```

It gets `rigscope@latest` from npm, backs up the hub database and replaces the
managed runtime. Accounts, history and collector settings are retained. Only
components that were running are restarted; startup settings stay the same.

This retention applies to installations written in the canonical format. On a
pre-canonical installation, the unfinished format is reset once rather than
migrated: the latest CLI stages and validates the replacement, stops the hub and
collector completely, then removes the old configuration and history and
installs fresh. Back up the state directory first if that history must be kept.
There is no migration and no rollback to the old format; `install` also resets
detected pre-canonical state.

An older installed CLI cannot stage a package without the removed configuration
format metadata. From a pre-canonical installation, run the latest CLI directly:

```sh
npx --yes rigscope@latest upgrade
```

Or remove the services and reinstall:

```sh
rigscope uninstall
npx --yes rigscope@latest install --boot
```

Both paths stop the owned services and reset the pre-canonical configuration and
history. Future upgrades that stay in the canonical format keep accounts, history,
settings and collector identities.

| Alternative | Command |
| --- | --- |
| Select an exact published version | `rigscope upgrade --from rigscope@0.1.4` |
| Install a local archive | `rigscope upgrade --from ./rigscope-VERSION.tgz` |
| Return to the retained previous runtime | `rigscope rollback` |

A fresh installation has no previous runtime to roll back to. Updating a global
npm package does not update this runtime. Do not move runtime directories or run
old code against a newer database. See [Upgrade and rollback checks](operations.md#managed-cli-operations)
for health checks, failure recovery, database compatibility and backup selection.
Keep separate backups outside the machine.

## Troubleshooting

### No password was printed

The `owner` password appears only when the account is created. Reinstalling with
saved settings keeps the old password. If you lost it, reset it at a private
prompt; Rigscope coordinates stopping and resuming the running installation:

```sh
rigscope recover-password --user owner
```

Recovery keeps history and collector credentials. Sign in again afterward.
It changes an existing account; it does not create a missing user or grant
administrator access. See [Password recovery](operations.md#password-recovery).

### An old command opens, or uninstall is unknown

If installation prints `Keeping existing command at …`, another launcher already
occupies that path. The installer leaves it intact and prints a usable `Command:`
path for the new CLI. Use that path, or invoke the published CLI directly:

```sh
npx --yes rigscope@latest status
npx --yes rigscope@latest uninstall
```

Choose the operation you need; `uninstall` deletes this installation and its
data. Use `uninstall --keep-data` to preserve saved data. Use `ls -l ~/.local/bin/rigscope` to check for a link to an older
checkout. Back up a confirmed old launcher before removing it, then repeat
`install` to create the new launcher. Older services with other names may need
separate cleanup; do not stop unrelated services.

### The hub keeps restarting

Check `rigscope logs hub` and `rigscope doctor`. If port 8081 is already in use,
identify the listener before stopping anything. For a fresh install, choose an
unused port with `install --port 8086 --boot`, or use HTTPS mode with a private
socket. To change a retained installation's local port, run
`rigscope configure --port 8086` and approve the stop prompt. Previously running
components resume after the change. `--origin` and `--port` cannot be combined.

## Hardware

GPU names use NVIDIA's reported model or the system's PCI name database when
available. Otherwise a vendor label is shown, with the PCI address separately.
A detected adapter can remain visible without telemetry: driver support and
permissions determine which counters can be read. Intel integrated adapters do
not necessarily expose utilization or dedicated VRAM through sysfs; shared
system RAM is not reported as dedicated VRAM.

History only adds historical GPU cards when stored series contain readings.
Empty PCI placeholder series do not create an extra card while NVIDIA's UUID
and model become available. Historical cards with readings remain accessible.

Collection supports Linux. CPU, memory and process readings come from procfs.
ARM and RISC-V CPU names are recognized; utilization uses per-core counters.

| Hardware | Available readings | Limit |
| --- | --- | --- |
| NVIDIA | Utilization, dedicated memory, temperature, power; compute-process memory | Requires `nvidia-smi` |
| AMD discrete GPU | Busy percentage, dedicated VRAM, temperature, power | Requires readable amdgpu sysfs/hwmon counters |
| AMD integrated GPU | Busy percentage and available hardware sensors | Reports only driver-provided VRAM; does not estimate shared memory |
| Intel discrete or integrated GPU | Available hwmon temperature/power and optional driver counters | Partial support; most utilization readings remain unavailable |
| ASPEED/other display adapters | Inventory | No telemetry backend |

On AMD APUs the reported power counter can include CPU/SoC power; it is not a
GPU-exclusive power measurement.

Drivers, permissions and kernel versions determine which readings are available.
Missing readings never become zeros. Intel temperature or power support does not
imply utilization or per-process GPU memory support. Inventory refresh detects
hotplug changes. AMD/Intel adapters use PCI-based IDs; NVIDIA uses GPU UUIDs.

AMD uses the kernel's [amdgpu sysfs and hwmon interfaces](https://docs.kernel.org/gpu/amdgpu/thermal.html).
Physical checks cover an Ubuntu RTX 3090 rig and a laptop with an RTX 5060 and
Radeon 860M, including iGPU utilization, memory, temperature and power stored by
the hub. Intel sensors and ARM/RISC-V naming have fixture tests but still need
physical validation. macOS, Windows and Apple GPU collection are not supported.


## System installation

For dedicated service accounts, system units and CPU package-power permissions,
see [System installation](system-installation.md). The default npm installation
above runs as your own user.
