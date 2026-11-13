# Installation

Start with the default local dashboard, then add remote machines when you need them.
A **hub** hosts the dashboard; a **collector** sends readings from a machine.

1. [Install](#before-you-install) the hub and local collector.
2. [Sign in as owner](#owner-password) at **http://127.0.0.1:8081**.
3. For more machines, [set up HTTPS](#remote-access-and-tls), then use
   **Administration → Add machine** and run its installer command on each machine.

Most users can keep the defaults. Use the options below for system services,
GPU temperatures or remote access.

## Before you install

You need:

- Linux with systemd (the user manager for the default scope).
- `amd64`, `arm64` or `armv7`. Other architectures are refused before download.
- `curl` or GNU `wget` (BusyBox `wget` is refused because it may not verify TLS certificates), `tar`, and `sha256sum` or `shasum` for the installer script.
- No Node.js, npm, Python, Docker or external database.

The installer downloads the release for your architecture and verifies its SHA-256 checksum.

```sh
curl -fsSL https://rigscope.dev/install.sh | sh
```

Arguments after `sh -s --` are passed to `rigscope install`:

```sh
curl -fsSL https://rigscope.dev/install.sh | sh -s -- --boot
```

## User scope and system scope

The default is a **user installation**. It runs as you, uses your systemd user
manager, and keeps everything under your home directory. It needs no root. User
services start at login unless `--boot` enabled lingering; `--boot` makes them
start at boot, before login.

A **system installation** (`sudo ... --system`) is root-owned, runs as
dedicated service accounts, and starts at boot. Use it for a shared dashboard or
a monitoring agent on a multi-user machine.

| | User scope (default) | System scope (`--system`) |
| --- | --- | --- |
| Binary | `~/.local/bin/rigscope` | `/usr/local/bin/rigscope` |
| Previous binary | `~/.local/bin/rigscope.prev` | `/usr/local/bin/rigscope.prev` |
| Config | `~/.config/rigscope/config.json` (0600) | `/etc/rigscope/config.json` (0640 `root:rigscope`) |
| State | `~/.local/state/rigscope/` (0700) | `/var/lib/rigscope/` (0755 `root:root`) |
| Units | `~/.config/systemd/user/` | `/etc/systemd/system/` |
| Service accounts | you | `rigscope-hub`, `rigscope-collector` |
| Startup | at login; at boot with `--boot` | at boot |
| Logs | journald user journal | journald system journal |

The state root is traversable in system scope because each component has a
private subtree: `hub/` is owned by `rigscope-hub:rigscope` and `collector/` by
`rigscope-collector:rigscope`, both mode 0700. The hub cannot read the
collector's enrollment grant or issued credential. Accounts and groups are
created by `systemd-sysusers`; the shared `rigscope` group exists so both
accounts can read the 0640 configuration. Neither service runs as root or keeps
capabilities.

`XDG_CONFIG_HOME` and `XDG_STATE_HOME` are honoured in user scope, but the
binary always lives at `$HOME/.local/bin/rigscope`.

## Install options

```sh
rigscope install [options]
```

| Option | Meaning |
| --- | --- |
| `--role hub\|collector\|both` | Initial component preset for a fresh install. Default `both`. |
| `--system` | Install root-owned system services. Requires root. |
| `--boot` | User scope only: enable `loginctl` lingering so services start at boot. System scope always starts at boot. |
| `--no-start` | Write everything but do not start services. |
| `--port PORT` | Hub listen port, 1–65535. Default 8081. |
| `--origin URL` | Canonical public hub origin, for example `https://hub.example.com`. Required when the hub listens on a non-loopback address. |
| `--hub URL` | Collector destination, the hub's WebSocket endpoint. Requires `--role collector` or `--role both`. |
| `--token TOKEN` | One-time enrollment grant for `--hub`. Both or neither. |
| `--hub-ca FILE` | Optional absolute PEM CA bundle that pins the hub certificate. Requires `--hub`. |
| `--password-stdin` | Read the first owner password from standard input instead of asking. Only for a hub that has never started. |
| `--yes`, `-y` | Approve stopping running services without asking, and generate the owner password instead of asking for one. |
| `--gpu-registers` | Collector only: opt in to NVIDIA PCI BAR0 register temperatures. System scope installs the `rigscope-gpu` group and udev rule. User scope installs the same root-owned rule through `sudo`; see [GPU register temperatures](#gpu-register-temperatures). |

Examples:

```sh
# Local dashboard and local collection
curl -fsSL https://rigscope.dev/install.sh | sh

# Dashboard only, no local collection
curl -fsSL https://rigscope.dev/install.sh | sh -s -- --role hub --boot

# This machine sends to a hub and hosts no dashboard
curl -fsSL https://rigscope.dev/install.sh | sh -s -- \
  --role collector \
  --hub wss://hub.example.com/api/collector/connect \
  --token 'GRANT'

# Machine-wide dashboard, written but not started (add TLS before starting)
curl -fsSL https://rigscope.dev/install.sh | sudo sh -s -- --system \
  --role both \
  --origin https://hub.example.com \
  --no-start
```

`install` is idempotent. Re-running it repairs the binary, units, enablement,
state directories and startup setting. It keeps existing configuration,
collector identity and credentials, and may add a component, but never silently
disables one that is already enabled. A collector-only install has the hub
component disabled; a hub-only install has the collector disabled.

The default install automatically pairs the local collector with the hub. If
pairing is pending or expired, rerun `rigscope install`; it preserves credentials
and repairs unfinished pairing. An explicit `--hub` destination is never overridden.

Automatic pairing requires a connection the hub can identify as local. A loopback
trusted proxy disables new automatic pairing; use `--hub URL --token TOKEN`
instead. Already-paired collectors keep working. See
[trusted proxies](configuration.md#componentshub) and
[Host and Origin checks](security.md#host-and-origin) for the network rules.

Before writing anything, `install` refuses to touch files it does not own and
refuses to run when it detects a Rigscope 0.2 installation.

### Roles

`--role` selects what a **fresh** installation enables:

- `hub`: dashboard and local hub database, no local sampling.
- `collector`: local sampling and delivery, no dashboard listener.
- `both` (default): both components on this machine.

On a re-run, `--role` can add a component but not remove one. To turn a
component off or on later, use `rigscope configure` (below).

### Choosing a role later

```sh
# Add local collection to a hub-only install; this pairs it automatically
rigscope configure --collector on

# Add a dashboard to a collector-only install
rigscope configure --hub on

# Stop collecting on this machine, or stop running the dashboard here
rigscope configure --collector off
rigscope configure --hub off
```

Turning a component off stops and disables its service and keeps its data, so
turning it on again picks up where it left off. At least one component must
stay on; to remove Rigscope use `rigscope uninstall`. `--hub on` for a hub that
has never run sets up the owner account the same way `install` does: choose a
password at the prompt, pass `--password-stdin`, or note the generated one it
prints. `install --role both` still adds a component too.

Enabling the collector later uses the same automatic pairing: the next
`install` or `configure` writes a fresh grant and the hashed bootstrap file because the
collector still has no destination. The already-running hub imports the pending
grant when the enrollment request arrives, so no hub restart is needed. If you
instead stage a specific grant with `install --hub URL --token TOKEN`, `install`
restarts an enabled collector after writing the grant so it is consumed
immediately rather than on the next manual restart. The automatic flow restarts
the collector too, so an active collector picks up its first destination.

`install --hub URL --token TOKEN` on a machine that already has a collector
never disables a component (`--role collector` does not remove a hub), and it
protects a collector that is already working:

- **Already enrolled with that hub**: `install` redeems the grant itself and
  the new credential replaces the stored one, restarting a running collector.
  The credential file on the machine cannot show whether the hub still accepts
  it (the machine may have been removed there), so it never causes the grant to
  be skipped. If the hub refuses the grant (for example because the collector is
  still registered: one installation runs one collector), the collector keeps
  its credential and `install` prints a warning explaining what to check.
- **The URL is this machine's own hub** (its origin, or a loopback address on
  its port): the collector keeps its direct local address instead of leaving
  through a proxy and coming back. Without a credential the grant is staged for
  that local address.
- **Enrolled with a different hub**: `install` asks before moving it (`--yes`
  approves), then redeems the grant with the new hub itself. Only when the new
  hub accepts does the configuration change. If the hub refuses the grant,
  install stops and the collector keeps sending to the hub it already works
  with.
- A collector that never enrolled with its current destination is not sending
  anywhere, so it is pointed at the new hub without asking, and the grant is
  staged for the service to redeem as on a new machine.

`install` over an older version says `Upgraded Rigscope from OLD to NEW`.

### GPU register temperatures

Enable optional NVIDIA junction and VRAM temperatures:

```sh
rigscope install --gpu-registers
```

This needs a root-owned udev permission rule; the collector still runs without
root. System installs use `rigscope-gpu`. User installs use a private primary
group when possible, otherwise the shared group.

- If asked to log out, log out fully and rerun the command. With lingering enabled,
  reboot or terminate your user manager as instructed by the installer.
- With a piped installer or unavailable sudo, run the printed commands manually,
  then rerun `rigscope install --gpu-registers`.
- Missing permissions or unsupported registers appear as unavailable readings.
  Register layouts are undocumented and card-specific; verify readings on your hardware.

<details>
<summary>Advanced: permission checks, manual setup and rule ownership</summary>

`--gpu-registers` is a collector-only opt-in for NVIDIA junction and VRAM
temperatures. The collector reads the PCI BAR0 registers through
`/sys/bus/pci/devices/<bdf>/resource0`, so the device node must be readable by
the account that runs the collector. That needs one root-owned udev rule:
`/etc/udev/rules.d/90-rigscope-gpu-temps.rules`. No group is created when the
rule can use your own private group, and the collector never runs as root.

In **system scope** the installer writes the rule as root and grants it to the
shared `rigscope-gpu` group, which the collector unit joins.

In **user scope** the install itself still runs as you, including the binary and
units it writes. Only the root-owned steps go through `sudo`, as plain
commands: the binary is never re-executed under sudo. The installer picks the
group the rule grants:

- If your primary group is private — its name equals your user name, it lists
  no other member, and no other account uses its GID as primary — the rule
  grants that group. Access works as soon as udev applies the rule, with no
  log-out. The private group is only used when `/etc/nsswitch.conf` serves
  `passwd` and `group` from local files (`files`, `systemd`, `compat` or
  `altfiles`) and your uid appears in `/etc/passwd`; with NSS such as LDAP or
  SSSD the local file cannot prove the account layout, so the shared group is
  used instead. Release builds are cgo-free, so when the uid is not in
  `/etc/passwd` the installer takes your name from `USER` (or `LOGNAME`) if it
  is a valid account name, and always uses the shared group: a name from the
  environment cannot prove a private group.
- Otherwise the rule grants the shared `rigscope-gpu` group. The installer adds
  you to the group when the group database does not already list you as a
  member, and creates the group only when it is missing. A user service gets its
  supplementary groups when the systemd user manager starts, so you must log out
  fully (not just close the desktop session) and then run
  `rigscope install --gpu-registers` again to verify and enable sampling. A
  readable `resource0` in a fresh ssh or tty session is not enough on its own:
  the installer also reads the `Groups:` line of your running systemd --user
  manager. When that manager is known and lacks the rule's group,
  `gpuRegisters` stays off. When the manager cannot be found or the group cannot
  be resolved, the installer cannot check the collector's session: it says so
  with a warning and still enables sampling if the register probe succeeds.
  If linger is enabled, logging out leaves the old manager running; the
  installer then says to reboot or to run `loginctl terminate-user <user>`
  yourself (it never runs that command).

The exact commands are printed before they run, with every path shell-quoted:

```text
sudo install -m 0644 -o root -g root '<rendered rule>' /etc/udev/rules.d/90-rigscope-gpu-temps.rules
sudo udevadm control --reload
sudo udevadm trigger --subsystem-match=pci --attr-match=vendor=0x10de
sudo udevadm settle
```

With the shared-group fallback, `sudo groupadd --system rigscope-gpu` (only when
the group is missing) and `sudo usermod -aG rigscope-gpu <user>` (only when you
are not already a member) run first. A reinstall where you already belong to the
group runs no group or membership command at all.

The installer then opens `resource0` read-only and maps its first page
`PROT_READ`/`MAP_SHARED`, then unmaps it without reading a register word, as the
collector's mapping would. That proves permission without performing MMIO, so a
suspended discrete GPU is never woken. Display functions whose
`power/runtime_status` is `suspended` are reported as present but not probed and
never fail the check. Sampling is enabled only when at least one NVIDIA display
device can be probed and, for the shared group, when the user manager is not
known to lack the group.

When stdin is not a terminal — a piped installer is the common case — or when
sudo fails or is declined, the commands are printed for you to run by hand and
the rest of the install completes with a warning. The rendered rule is kept in
your state directory (`<stateDir>/gpu-registers.rules`, user-owned, mode 0644
and persistent), not in `/tmp`, so the printed path still exists later. On a
home directory mounted over NFS with `root_squash`, the printed `sudo install`
may fail to read that path because root is mapped to another user; copy the file
to `/tmp` first and point the command at the copy. The manifest records the rule
as `planned`, never `owned`, `gpuRegisters` stays `false`, and nothing outside
your home is changed.

An existing rule is never taken over from another account:

- A rule that grants the group this install needs — your private group, or the
  shared `rigscope-gpu` group under the fallback — is adopted, whether or not it
  carries a Rigscope marker and whether or not it is readable yet. No rewrite
  happens and the rule is recorded as `adopted`; uninstall leaves it in place
  and prints how to remove it.
- A byte-identical 1.0 rule for your own private group is instead recorded as
  `owned`, so a rule you installed by hand from the printed commands is removed
  by uninstall. That is the only case a user install owns the rule.
- A rule for `rigscope-gpu` is always recorded as `adopted`, even when this user
  install wrote it, because every account in the group may rely on it. Uninstall
  leaves it in place. A shared rule is never rewritten to your private group,
  even when you have one: it belongs to the group (a system install or 0.2 wrote
  it), so the installer adopts it and, only if you are not already a member,
  runs `sudo usermod -aG rigscope-gpu <user>` plus the log-out guidance.
- A rule that grants some other group — another user's private group, or a
  foreign rule — is left alone with a warning. `gpuRegisters` stays `false` and
  the rest of the installation completes.
- A reinstall compares the file on disk with what the manifest recorded (digest
  and group). If either changed — for example a system install rewrote the rule
  for `rigscope-gpu` — the entry is demoted to `adopted` and uninstall will not
  delete it.

Uninstall deletes the rule only when the entry is `owned`, the file still
carries the Rigscope marker, its content still matches the recorded digest, and
it grants your private group. Anything else is left in place with the command
to remove it. Rigscope never removes groups or accounts.

</details>

### Owner password

`install` creates the hub's `owner` account before the hub first starts. On a
terminal it asks for a password (typed twice, not echoed); press Enter to have
one generated instead. A generated password is printed once at the end of the
install output:

```text
  open:    http://127.0.0.1:8081
  sign in: owner / <generated>
  Save this password; it is shown once. To change it: rigscope reset-password
```

A password you chose is never printed. `--password-stdin` reads it from
standard input instead (`rigscope install --password-stdin < file`; not through
`curl | sh`, whose standard input is the script). `--yes`, or no terminal at
all, generates one. The password is never written to the journal; the hub
stores only its argon2id hash. Reinstalls and upgrades keep the existing
accounts and never ask again.

To change it, or if it is lost, reset it offline:

```sh
rigscope reset-password
```

The reset needs the hub stopped. `reset-password` first asks for the new
password the same way (Enter generates one; `--password-stdin` reads it from
standard input), then, when the hub service is running, says so and asks before
stopping it. It sets the password for `owner` (or `--user NAME`), revokes that
account's sessions and starts the hub again. `--yes` (`-y`) approves without asking and
`--no-restart` leaves the hub stopped. A hub started outside systemd, such as
`rigscope hub` in a terminal, must be stopped by hand first. The command never
reveals or replaces another password.

## Everyday commands

```sh
rigscope status [--json] [--system]
rigscope start|stop|restart [hub|collector] [--system]
rigscope logs [hub|collector] [--follow] [--system]
rigscope doctor [--system]
```

`--system` is optional on these and on every other command that acts on an
existing installation (`configure`, `reset-password`, `collector enroll`,
`upgrade`, `rollback`, `uninstall`, and `install` run again to change or repair
one). Re-running `install` changes the installed components only when you pass
`--role`. Without it Rigscope uses the per-user
installation, or the system installation when there is no per-user one, and
says so. Commands that change a system installation need root; run without
`sudo`, they re-run themselves through `sudo`, which asks for your password.
With no terminal to ask on, they say to run again with `sudo`.

`start` reports a unit that is already running instead of starting it again;
use `restart` to apply new settings or credentials. `collector enroll` restarts
a running collector service (after asking, or with `--yes`) so it uses the new
credential at once.

- `status` prints the scope, configuration and state paths, whether the binary
  and `rigscope.prev` exist, and each component's enabled/active state, including
  the collector's destination count. `--json` prints the same report as one JSON
  document.
- `start`, `stop` and `restart` act on every enabled component when no component
  is named. `start` and `restart` require the unit to be installed. These
  commands take the installation lock, so they cannot race an install or an
  upgrade.
- `logs` shows the last 100 journal lines for the selected components and adds
  `--follow` when asked. It is `journalctl --user` in user scope and the system
  journal in system scope.
- `doctor` checks the ownership manifest, configuration, file modes and
  ownership, state directories, binaries, units, collector destination and, in
  system scope, the sysusers and udev integration files. It exits nonzero when a
  check fails. The RAPL and GPU register checks report what is readable by the
  account running `doctor` (named in the result). In system scope a readable
  file is not enough: `doctor` also requires the file's group to appear in the
  system collector unit's `SupplementaryGroups`, so root access alone is never
  reported as collector access.

Every command prints errors as `rigscope: ...` and exits nonzero on failure.
Output is colored on a terminal; set `NO_COLOR=1` to turn that off.

### Commands that stop running services

`install` (when it runs again over a running installation), `configure`,
`upgrade`, `rollback`, `reset-password` and `uninstall` never refuse because a
service is running, and never leave that to you. Each one names the services
it must stop or restart and asks once:

```text
:: The hub and collector are running and will restart so the changes take effect.
Continue? [y/N]
```

Answering no changes nothing. After the operation, only the services that were
running are started again; `uninstall` never starts anything. `--yes` (`-y`)
approves without asking, for scripts. The question is asked on the terminal, so
it also works under `curl ... | sh`. Without a terminal and without `--yes`,
the command stops before changing anything and tells you to add `--yes`.

`install --no-start` never stops or restarts running services: it writes the
new files and warns that the running services keep the previous binary and
settings until `rigscope restart`.

## Change settings

Look at the current settings, then change what you need:

```sh
rigscope configure --show
rigscope configure --port 9000
rigscope configure --telemetry-mode persistent
rigscope configure --share process-arguments=on --share disks=off
rigscope configure --sampling-interval 5s
rigscope configure --origin https://hub.example.com:8443
rigscope configure --trusted-proxy 127.0.0.1
rigscope configure --local
```

Every change works the same way:

1. `configure` prints the effective change (`old -> new`) and anything you
   should know, such as a higher CPU cost or history that will be lost.
2. It checks the complete result first. A value the hub or collector would
   refuse stops here, before anything is written or stopped.
3. If a running service must restart or stop, it says which and asks once.
   `--yes` approves; `--no-restart` stops the affected services and leaves them
   stopped. Turning on sensitive sharing (`network-addresses`,
   `process-arguments`) always asks, even when nothing restarts.
4. It saves only the settings you named and restarts only the component they
   affect. Running it with the current value changes nothing.

| Change | Flags | Restarts |
| --- | --- | --- |
| Dashboard port, public origin, local only again | `--port PORT`, `--origin URL`, `--local` | hub (a paired local collector follows) |
| Listen address and HTTPS | `--listen ADDRESS`, `--tls-cert FILE --tls-key FILE`, `--no-tls` | hub (see [Remote access and TLS](#remote-access-and-tls)) |
| Reverse proxy trust | `--trusted-proxy LIST\|none`, `--forwarded-headers x-forwarded\|forwarded` | hub |
| History storage | `--telemetry-mode memory\|persistent` | hub |
| What this machine shares | `--share FAMILY=on\|off`, repeatable or comma-separated | collector |
| How often it samples | `--sampling-interval DURATION`, 100ms to 1h | collector |
| Stop GPU register temperatures | `--gpu-registers off` | collector |
| Run a component here or not | `--hub on\|off`, `--collector on\|off` | that component |

Sharing families are `cpu`, `memory`, `swap`, `gpus`, `disks`, `network`,
`network-addresses`, `processes` and `process-arguments`; see the
[sharing policy](configuration.md#sharing-policy). `network-addresses` needs
`network` on and `process-arguments` needs `processes` on. Collector settings
work on a collector-only machine; hub flags need `--hub on` there first.
`--gpu-registers off` keeps the udev rule and group access, so
`rigscope install --gpu-registers` turns sampling back on and uninstall removes
them. `--show --json` prints the settings for scripts; it never contains
grants, credentials or passwords.

A collector on the same machine that the installer paired with this hub moves
with it: its destination and credential are rebound to the hub's new local
address, with no new enrollment. Setting an origin keeps it on its loopback
address. Collectors on other machines keep the old address; give each one a
Replacement grant for the new URL. Use the installed binary (`rigscope
configure`); a binary of another version refuses.

Administrators can also see the running hub's settings and storage use in
Administration → Hub settings. The dashboard cannot restart the hub, so its
storage form gives you the exact `configure` command to run on the hub machine.

An HTTPS origin needs something that answers at it. When the hub does not
serve TLS itself and no trusted proxy is configured, `install` and `configure`
say so and print the next steps. For example, to reach a hub at
`https://laptop.example.ts.net:8081` over Tailscale:

```sh
rigscope configure --origin https://laptop.example.ts.net:8081
sudo tailscale serve --bg --https=8081 http://127.0.0.1:8081
rigscope configure --trusted-proxy 127.0.0.1
```

`tailscale serve` terminates HTTPS with the tailnet certificate, keeps the
`Host` header and sends `X-Forwarded-For`, `-Proto` and `-Host`, which is what
the [reverse-proxy contract](configuration.md#the-reverse-proxy-contract)
asks for. Trusting `127.0.0.1` lets the hub believe it, so sign-in cookies are
marked `Secure`.
`another rigscope operation is running` means another install, upgrade,
uninstall or service command holds the installation lock.

For durable history, use `rigscope configure --telemetry-mode persistent`.
Fresh installs use `memory`; switch back with `--telemetry-mode memory`.
The command warns before restarting: RAM history is not copied between modes.
Collector buffering remains RAM-only. See [Telemetry storage](telemetry-storage.md).

## Remote access and TLS

By default the hub binds `127.0.0.1:8081` and accepts loopback clients only.
To serve HTTPS to other machines with your own certificate:

```sh
sudo rigscope configure --system --listen 0.0.0.0 --port 8443 \
  --origin https://hub.example.com:8443 \
  --tls-cert /etc/rigscope/tls/fullchain.pem --tls-key /etc/rigscope/tls/privkey.pem
```

Before changing anything, `configure` checks that the certificate and key load
as a pair, that the certificate has not expired and covers the origin's name,
and, for a system install, that the `rigscope-hub` service account can read
both files. Files under `/home`, `/root` or `/tmp` are refused there because the
hub service cannot see those directories. The hub reads the certificate when it
starts: after each renewal, run `rigscope restart hub`. `--no-tls` stops serving
HTTPS; add `--listen 127.0.0.1 --local` to go back to local-only access, or
`--trusted-proxy ADDRESS` when a reverse proxy serves HTTPS instead.

The resulting hub section looks like this:

```json
"hub": {
  "enabled": true,
  "listen": "0.0.0.0",
  "port": 8443,
  "origin": "https://hub.example.com:8443",
  "tlsCert": "/etc/rigscope/tls/fullchain.pem",
  "tlsKey": "/etc/rigscope/tls/privkey.pem"
}
```

This is the `components.hub` object; the command preserves the other settings.
The certificate must name `hub.example.com`. Browser pages, sign-in and API
requests must use the origin's authority (`hub.example.com:8443` here); other
Host values get HTTP 421. Collector endpoints have a narrow exception for
direct loopback connections with a loopback Host at the hub's port and no
forwarding headers. This exception does not bypass TLS certificate checks.

### Keep the local collector connected

The `configure` command above also handles the default auto-paired collector
when it still uses the installer's previous automatic loopback destination.
It updates that destination and rebinds its credential to the new address,
then asks before restarting affected running services. No configuration edit,
replacement grant or separate restart is needed for that case. For direct TLS,
the automatic destination uses the origin hostname and trusts the hub's
certificate.

The hostname must resolve to an address this machine can reach. For automatic
pairing it must resolve to one of this machine's own addresses. If public DNS
requires unsupported NAT hairpinning, configure local DNS or a hosts entry such
as `127.0.0.1 hub.example.com`; the Host header and certificate name remain
`hub.example.com`.

### Reconnect other collectors

An independently configured destination, including a collector already moved
to a non-loopback hostname, is preserved. Remote collectors also need updating
when the hub address changes. Create a **Replacement grant** for the collector
in Administration → Collectors, then run this on its machine:

```sh
rigscope install --hub https://hub.example.com:8443 --token 'GRANT'
```

This updates the destination and enrollment together, preserves the installed
role and unrelated settings, and asks before restarting running services. For
a private or self-signed CA, add `--hub-ca /path/to/ca.pem`; omit it for a
publicly trusted certificate. Use `sudo rigscope install --system` with the
same destination options for a system installation.

Prefer this complete workflow when moving a collector. The lower-level
`collector enroll` command stores credentials but expects the configuration's
destination to match already.

A non-loopback listener is refused unless it has TLS or a configured trusted
reverse proxy. A plaintext listener admitted by `trustedProxies` accepts TCP
connections only from those proxies and from loopback. TLS uses your own
certificate; Rigscope does not obtain or renew certificates and does not serve
ACME challenges. Minimum protocol is TLS 1.2 with ECDHE AEAD suites.

For a reverse proxy, see the
[reverse-proxy contract](configuration.md#the-reverse-proxy-contract) and set
`origin`, `trustedProxies` and the matching `forwardedHeaders` family. The proxy
must forward WebSocket upgrades for `/api/collector/connect`, stream SSE without
buffering for `/api/live`, and allow long-lived responses. Browsers need no
direct access to collectors, and collectors never accept inbound connections.

## Upgrade

```sh
rigscope upgrade [--version VERSION] [--system] [--yes]
```

Without `--version`, the latest published GitHub release is used. The command
downloads `rigscope_<version>_linux_<arch>.tar.gz` and `checksums.txt`, verifies
SHA-256, extracts only the `rigscope` binary, and performs an activation
transaction:

1. Take the installation lock, ask before stopping the running components
   (`--yes` approves) and quiesce them.
2. Back up the database, configuration and collector identity as one snapshot.
3. Run configuration and database migrations with the new binary.
4. Replace the binary atomically and retain the old inode as `rigscope.prev`.
5. Start enabled components and verify health.

Health means `GET /api/health` answers and every enabled unit is active, retried
20 times at 500 ms intervals. If that fails before the new release has accepted
writes, Rigscope restores the backup and the previous binary and exits nonzero.
A later `rigscope upgrade` detects an interrupted transaction and resumes or
rolls it back rather than guessing.

The previous binary is kept for manual rollback. Backups live in the state
directory under `backups/`; the newest three completed snapshots are kept, plus
any snapshot a pending transaction still needs.

Upgrade order is hub first, then collectors. Protocol compatibility is
versioned: a hub accepts the current and previous protocol version, and shows a
collector that is too old as needing an upgrade. Do not upgrade collectors
before the hub.

A development build reports `dev`; only release builds short-circuit with
"already installed".

### Rollback

```sh
rigscope rollback [--system] [--yes]
```

`rollback` asks before stopping running components (`--yes` approves), swaps
`rigscope.prev` back, restarts enabled components and runs the
same health check. It is a transaction too: if it cannot quiesce, restore data
or pass the restored health check, it stops with everything kept for an operator
instead of discarding an unknown state.

Rollback is limited by the **schema floor**. The data records the oldest release
that can safely open and write it. A rollback is allowed only when the release
you return to is at or above that floor. If the older release predates a
breaking schema migration, Rigscope refuses and leaves the current installation
and data untouched. This is deliberate: running old code against new data is
worse than staying on the new release.

A rollback also drops a pending pairing binding. A release that predates the
pairing-grant binding columns imports the hub's bootstrap file without the
purpose and collector id the grant is bound to, and this release then refuses to
redeem such a grant rather than treat it as an administrator-issued one. Re-run
`rigscope install` after the rollback to stage a fresh, bound pairing grant; the
saved recovery proof is kept.

### What an upgrade preserves

Within 1.x, upgrades preserve accounts, passwords, sessions, collector
identities and credentials, configuration, machine names and tags, alert rules
and incidents, and history. Layout paths and file names do not change. A 1.x
binary refuses a configuration written by a newer 1.x instead of discarding
fields.

## Uninstall

```sh
rigscope uninstall [--keep-data] [--system] [--yes]
```

Uninstall requires a valid 1.x ownership manifest. Without it, nothing is
stopped or deleted, even if the default state directory looks like an old
installation.

With a valid manifest it asks before stopping running services (`--yes`
approves), then stops and disables every recorded unit, verifies they
are inactive, then removes only paths it recorded as owned: units, the binary
and `rigscope.prev`, transaction temp files, configuration, and the state
directory including history, credentials, the upgrade journal and backups. It
also removes the sysusers file and udev rules in system scope and reloads udev.
This includes `/etc/sysusers.d/rigscope.conf`, the CPU package-power rule
`/etc/udev/rules.d/90-rigscope-rapl.rules`, and the optional NVIDIA junction/VRAM
temperature rule `/etc/udev/rules.d/90-rigscope-gpu-temps.rules`. Hardware-access
configuration is removed even with `--keep-data`.
Service accounts and groups are left in place for the administrator to remove.

A user installation also removes its owned private-group GPU rule through
`sudo`. If that removal or the udev reload cannot complete, uninstall exits
nonzero and retains the local binary, configuration and ownership manifest so
the cleanup can be retried. Without a terminal it prints the required root
commands; run them and retry uninstall from a terminal. Shared, adopted or
modified rules are preserved with an explanation and manual removal commands.

`--keep-data` keeps `config.json` and the state directory so a later install can
reuse accounts, history and collector identity. It still removes services and
the binary, and rewrites the manifest so a later uninstall cannot claim a file
that appeared at a removed path.

Removing a udev rule does not undo group and mode changes already applied to
existing sysfs files. A reboot, or the next device creation, restores kernel
defaults.

Uninstall is deliberately conservative: if a parent directory was replaced by a
symlink, if a recorded identity changed, if the state tree contains a mount
point, or if any unit cannot be stopped, it aborts before deleting anything.

### Leftover hardware setup from 0.2

After uninstalling 0.2, a system `install` recognizes unchanged default 0.2
udev rules. It previews and confirms replacement of rules the new installation
needs, writes the 1.x versions atomically, and records ownership for uninstall.
Unused default rules are kept without blocking installation or claiming
ownership. Modified rules still require inspection.

To remove unused default CPU-power and GPU-temperature access files instead,
use the optional cleanup command:

```sh
rigscope cleanup-legacy-hardware
sudo rigscope cleanup-legacy-hardware --apply
```

The first command changes nothing. The second removes only unchanged default
0.2 templates at these fixed paths and reloads udev:

- `/etc/udev/rules.d/90-rigscope-rapl.rules`
- `/etc/udev/rules.d/90-rigscope-gpu-temps.rules`
- `/etc/sysusers.d/rigscope-rapl.conf`

No 1.x installation manifest is required. Apply refuses detected Rigscope
service files, 1.x manifests or running hub/collector processes. It also refuses
symlinks, modified files, custom-group templates and 1.x rule contents before
deleting any candidate. If a file needs manual inspection, resolve it and run
the command again. If the udev reload fails after deletion, rerun `--apply` to
finish; this also works when all three files are already absent.

Accounts, group memberships and application data are kept. This command does
not restore old sysfs ownership immediately; reboot to restore kernel defaults.
Use the normal 1.x uninstaller for hardware setup owned by a 1.x installation.

## Upgrading from 0.2

Rigscope 1.0 is a new Go implementation. **There is no 0.2 data importer.**
Accounts, history, recordings, settings and collector credentials from the npm
version are not read.

1. If you want to keep the old files for reference, copy the 0.2 state
   directory somewhere private first. It will not be imported.
2. Remove the 0.2 installation with 0.2's own uninstaller:

   ```sh
   npx --yes rigscope@latest uninstall
   ```

   Remove the installation normally. Do not leave 0.2 configuration or state
   behind (including with `--keep-data`): the 1.0 installer detects those files
   and refuses to proceed until they are gone.
3. Install 1.0 fresh with the one-line installer.
4. Choose or note the owner password the installer shows, sign in, then enroll each
   collector from the hub's **Add machine** grant.
5. Confirm `rigscope version`, `rigscope status` and `rigscope doctor`.

`install` scans both scopes before writing anything. If it finds 0.2 user or
system units, `/opt/rigscope`, or 0.2 state files, it names them and refuses:

```text
a Rigscope 0.2 installation is present (/home/alice/.config/systemd/user/rigscope-6629b1795b8f-hub.service, ...)

Rigscope 1.0 does not import 0.2 data. Remove the old installation first:
  rigscope uninstall          # part of 0.2, or: npx --yes rigscope@latest uninstall
Then run this installer again
```

The npm package may be marked deprecated, but 1.0 is not published to npm and
`npx rigscope` never installs 1.0.

## Troubleshooting

### I never saw the owner password

The installer prints a generated password once, at the end of its output. If it
scrolled away, run `rigscope reset-password` and choose a new one. Passwords are
one-way hashes; there is no way to recover the old value.

### "another rigscope operation is running"

Another command holds the installation lock. Wait for it to finish. A stale lock
file does not need deleting: the kernel releases the lock when the process
exits, and the file normally stays in place.

### Install refuses a path

`install` will not overwrite a file it did not create. The error names the path.
Inspect it, move it aside, and retry. A marker alone does not prove ownership.
The system installer handles unchanged default 0.2 udev rules automatically;
other files without an ownership manifest are treated as foreign.

### Install says a 0.2 installation is present

Follow [Upgrading from 0.2](#upgrading-from-02). Remove the exact files listed
with 0.2's uninstaller, then rerun.

### The hub will not start

Common causes, all reported on stderr and in the journal:

- `origin is required when listening on non-loopback` — set a canonical
  `components.hub.origin`.
- `refusing to bind non-loopback ... without TLS or a configured trusted reverse
  proxy` — add `tlsCert`/`tlsKey` or `trustedProxies`.
- `origin ... is not canonical` — lowercase the host and remove a default port
  or trailing slash.
- `unknown field` — configuration is strictly parsed. Fix the spelling; a typo
  is never ignored.
- `configuration version N is newer than this binary supports` — upgrade
  Rigscope before using that file.

### The collector stays "awaiting enrollment"

A default `--role both` install creates its local pairing grant automatically
and that grant lives for one hour, long enough for a slow first hub start. The
grant is bound to the local collector's installation id and is redeemable only
from a socket the hub attributes to its own machine: loopback, or one of the
host's own addresses when the destination is the hub's origin. A request that
arrived through a reverse proxy, or from a configured trusted proxy, is refused.
The hub imports the hash when it starts, or on the first enrollment request when
it was already running, so adding a collector to an active hub needs no restart.
If the grant expired before the collector started (for example `--no-start` and
a start much later) or an install was interrupted, rerun `rigscope install`; it
stages a fresh grant and keeps the saved recovery proof. `rigscope doctor`
distinguishes the states: `pairing pending — the hub has not imported the staged
grant yet` means the staged file is still valid, `pairing pending — the hub has
imported the staged grant and the collector has not redeemed it yet` means the
import happened and the collector is the part to look at, and `pairing expired —
rerun rigscope install` means the one hour has passed. It only suggests a
reinstall while the hub component is enabled locally; a disabled hub, a loopback
trusted proxy, or a stale credential for another local destination each get their
own explanation, because rerunning `install` cannot repair those. An
operator-staged grant from `--hub`/`--token` is single-use and expires after 15
minutes; create a new grant from the hub if it ran out. Check
that `--hub` is the hub's address (`https://host`, which `install` normalizes to
`wss://host/api/collector/connect`) or that WebSocket endpoint itself, and that
it is not plaintext unless the hub is on literal loopback. If a grant is
already staged, retry the enrollment explicitly:

```sh
sudo rigscope collector enroll --system
```

`rigscope collector --check` prints whether the collector is disabled, awaiting
enrollment, has a staged grant, or is enrolled, plus its destination and state
directory. Add `--system` when checking a system installation.

### The dashboard is reachable locally but not remotely

Local `http://127.0.0.1:8081` working proves the process is healthy, not that
remote access is configured. Check `listen`, `origin`, TLS or `trustedProxies`,
the proxy's forwarded-header family, and that the proxy preserves the canonical
`Host` header. See [TLS and trusted proxies](configuration.md#tls-and-trusted-proxies).

### A service keeps restarting

Start with `rigscope doctor` and `rigscope logs --follow`. A configuration that
no longer loads, a non-loopback listener without TLS/proxy, a port already in
use, or a state directory with the wrong owner will all fail startup. In system
scope the units run as `rigscope-hub` and `rigscope-collector`, not root; a
permission error on a hand-made certificate or CA file is common.

### Rollback is refused

The current data is newer than the previous release can understand (the schema
floor). Do not force it. Use `rigscope upgrade --version` to select a release that
supports the data, or restore the whole installation from a backup snapshot.
