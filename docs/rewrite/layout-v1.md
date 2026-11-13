# Rigscope 1.0 installation layout

Implemented by `rigscope install`. This file is the 1.0 contract for paths,
ownership, configuration and upgrade behaviour. For user-facing setup and
service commands, see [Installation](../installation.md).

Rigscope 1.0 is one static Go binary. A release archive contains exactly one
file named `rigscope`; the binary is both the installer/CLI and the hub and
collector runtime. systemd supervises it directly. There is no Node.js, npm,
wrapper process or downloaded runtime tree.

## Scopes

| | User scope (default) | System scope (`--system`, root) |
| --- | --- | --- |
| Binary | `~/.local/bin/rigscope` | `/usr/local/bin/rigscope` |
| Previous binary | `~/.local/bin/rigscope.prev` | `/usr/local/bin/rigscope.prev` |
| Config | `~/.config/rigscope/config.json` | `/etc/rigscope/config.json` |
| State | `~/.local/state/rigscope/` | `/var/lib/rigscope/` |
| Units | `~/.config/systemd/user/` | `/etc/systemd/system/` |
| Service accounts | the invoking user | `rigscope-hub`, `rigscope-collector` |
| Unit names | `rigscope-hub.service`, `rigscope-collector.service` | same |
| Startup at boot | optional `loginctl enable-linger` (`--boot`) | always, once enabled |
| Logs | journald (user journal) | journald (system journal) |

`XDG_CONFIG_HOME` and `XDG_STATE_HOME` are honoured for user scope. The binary
still lives at `$HOME/.local/bin/rigscope`. User state stays mode `0700`;
configuration stays mode `0600` because it contains collector credentials.
System scope uses a shared `rigscope` group and mode `0640` so both service
accounts can read it.

### User layout

```text
~/.local/bin/
  rigscope                       0755, current release
  rigscope.prev                  0755, retained for rollback (after an upgrade)
~/.config/rigscope/
  config.json                    0600
~/.config/systemd/user/
  rigscope-hub.service           0644, managed by Rigscope
  rigscope-collector.service     0644, managed by Rigscope
~/.local/state/rigscope/         0700
  install-manifest.json         0600, ownership record written by install
  upgrade-journal.json          0600, format 2 upgrade transaction record
  hub.lock              0600, hub process lease (user scope)
  collector-pairing.json        0600, hashed automatic pairing grant; hub imports once, deletes
  backups/<name>/               0700, pre-upgrade database/config/identity snapshot
  staging/<version>/rigscope    0700 directory, verified staged release binary
  hub.db                         hub history (SQLite, written by the hub)
  hub.db-wal, hub.db-shm        SQLite WAL set, backed up and restored together
  collector/                    0700, collector-private directory
    collector-runtime.lock      0600, collector process lease
    enrollment.json             0600, transient one-time enrollment grant
    credentials.json            0600, issued per-collector credential
    identity.json               0600, persistent collector installation id
```

In user scope the hub and collector share the state root, so the hub's own
process lease (`hub.lock`) lives at the root. In system scope each
component has a private subtree and every runtime lease, credential and lock
lives inside the subtree the component account owns; nothing a component must
write is placed in the root-owned state root.

The installation lock is **not** in the state directory: it is
`$XDG_RUNTIME_DIR/rigscope/install.lock`, or `/tmp/rigscope-<uid>/install.lock`
when `XDG_RUNTIME_DIR` is unset. Uninstall deletes the state directory while
holding the flock, and removing a locked file would let a concurrent command
create a second inode with its own lock.

The manifest is the proof of ownership. `install` writes it before touching
any installation file; `uninstall` refuses without a valid 1.x manifest and
deletes only the paths recorded in it. The enrollment grant and the issued
credential live only in `collector/`, never in `config.json`.

### Manifest format 2

The manifest (`marker: "rigscope-installation/1"`, `formatVersion: 2`) records
each path with a state:

- `planned` — install intends to create this path but has not proved it did.
  Planned paths are never deleted.
- `owned` — install created the path with no-overwrite semantics and recorded
  its identity (device, inode, owner, group) and its parent directory's
  identity. Regular files also record a SHA-256 digest.
- `adopted` — install relies on a file it did not write, such as a working
  `90-rigscope-gpu-temps.rules` left by a 0.2 installation, a shared rule
  another installation wrote, or a `rigscope-gpu` rule a user installation
  itself wrote (every account in the group may rely on it). Adopted paths are
  never deleted.

A user-scope `udev-gpu` entry records the rule's group in `ruleGroup` when it is
`owned`. Only a rule for the user's own private group may be owned. A reinstall
that finds the file's digest or group changed demotes the entry to `adopted`, and
uninstall deletes it only while the entry is owned, the file carries the
Rigscope marker, its digest matches, and it still grants the private group.

Install writes a manifest of planned entries first, then promotes an entry to
`owned` only after the file or directory exists. Before publishing a file it
records the fully written staging temp path and digest in the entry
(`stagedPath`, `stagedSha256`). If the process dies between publication and
promotion, a retry adopts the file at the target only when its digest still
matches that record; otherwise it is treated as foreign. Re-running install
resumes planned entries; a planned path that already exists is treated as
foreign and refused. Uninstall deletes an owned path only while its current
identity still matches, so an interrupted install can never claim a file it did
not create.

The only authorization to delete a path is an `owned` manifest entry whose
identity matches. A planned entry and a reserved filename (`rigscope.prev`,
`upgrade-journal.json`, `staging/`, `backups/`, a transaction temp name) never
authorize deletion on their own.

The manifest records these activation artifacts; `upgrade` and the activation
coordinator create them and record each one as owned under the same
installation lock that install and uninstall use:

| Kind | Path | Created by |
| --- | --- | --- |
| `previous-binary` | `<binary>.prev` | `upgrade`, then re-recorded after a rollback restores it |
| `upgrade-journal` | `<stateRoot>/upgrade-journal.json` | `upgrade`, re-recorded after every atomic journal write |
| `staging-dir` | `<stateRoot>/staging/` | install creates the root; `upgrade` records it before staging a release |
| `backups-dir` | `<stateRoot>/backups/` | install creates the root; `upgrade` records it after writing a snapshot |
| `collector-state` | `<stateRoot>/collector/` | install owns it; a backup restore replaces its inode and re-records it |
| `binary-temp` | one transaction temp beside the binary, e.g. `.rigscope.<token>.new`, `.rigscope.<token>.prev`, `.rigscope.<token>.restore-new`, `.rigscope.<token>.restore-old` | `upgrade` records the prepared binary before publication; recovery forgets temps it removes |
| `binary-temps` | the binary directory itself | install records the shared parent directory; it is never deleted |

The collector directory is an ownership boundary for its private contents:
`enrollment.json`, `credentials.json` and `identity.json` are created, consumed
or replaced by the collector runtime, so their individual inodes legitimately
change. They are deleted (still descriptor-anchored and mount-checked) as part
of the owned `collector/` directory, never by trusting a recorded inode. The
same rule covers files inside the owned `staging/` and `backups/` directories.

Logs are not written to the state directory; both units use
`StandardOutput=journal` with `SyslogIdentifier=rigscope-hub` and
`rigscope-collector`.

### System layout

```text
/usr/local/bin/rigscope          0755, current release
/usr/local/bin/rigscope.prev     0755, retained for rollback
/etc/rigscope/config.json        0640, root:rigscope
/etc/rigscope/                   0755, root:root (traversable)
/etc/systemd/system/
  rigscope-hub.service           0644, root:root
  rigscope-collector.service     0644, root:root
/etc/sysusers.d/rigscope.conf    0644, service accounts and installed hardware groups
/etc/udev/rules.d/
  90-rigscope-rapl.rules         0644, package energy counters (collector role)
  90-rigscope-gpu-temps.rules    0644, only with --gpu-registers
/var/lib/rigscope/               0755, root:root (traversable)
  install-manifest.json          0600, root:root
  upgrade-journal.json           0600, root:root
  backups/<name>/                0700, root:root pre-upgrade snapshot
  staging/<version>/rigscope     0700 directory, verified staged release
  hub/                           0700, rigscope-hub:rigscope
    hub.db, hub.db-wal, hub.db-shm
    hub.lock             0600, rigscope-hub:rigscope process lease
    collector-pairing.json       0600, hashed automatic pairing grant; hub imports once, deletes
  collector/                     0700, rigscope-collector:rigscope
    collector-runtime.lock       0600, rigscope-collector:rigscope process lease
    enrollment.json              0600, rigscope-collector:rigscope
    credentials.json             0600, rigscope-collector:rigscope
    identity.json                0600, rigscope-collector:rigscope
/run/rigscope/install.lock       0600, root:root, installation flock
```

`systemd-sysusers /etc/sysusers.d/rigscope.conf` creates two unprivileged,
non-login accounts and the shared `rigscope` group. Both accounts join
`rigscope` so they can read the `0640` configuration file. Both accounts exist
even when one component is disabled so it can be enabled later without
reinstalling. The hub is never a member of a hardware group. The hub and
collector state directories are owned by their account with group `rigscope`
and mode `0700`, so the hub cannot read the collector's enrollment or issued
credential.

In system scope, the collector role installs the `rigscope-rapl` group and
RAPL rule by default. User installations leave RAPL permissions to the operator. PCI BAR0 register access is opt-in: only `install --gpu-registers`
creates the GPU udev rule and, in system scope, the `rigscope-gpu` group and
the collector's supplementary group. A `--role hub` install creates neither
hardware group nor udev rule.

A user installation cannot create a group or the collector unit's
supplementary groups. `install --gpu-registers` therefore runs only the
root-owned steps through `sudo` (as plain commands, never by re-executing the
user-writable binary) and renders the same rule at
`/etc/udev/rules.d/90-rigscope-gpu-temps.rules`:

- When the user's primary group is private (name equals the user, no other
  members, no other account has that GID as primary, `/etc/nsswitch.conf` uses
  only local sources and the uid is in `/etc/passwd`), the rule grants that
  group and access works immediately.
- Otherwise it grants the shared `rigscope-gpu` group; the installer runs
  `sudo groupadd --system rigscope-gpu` only when the group is missing and
  `sudo usermod -aG rigscope-gpu <user>` only when the account is not already a
  member, and the user must log out fully before rerunning
  `install --gpu-registers` to verify and enable sampling. The installer also
  reads the running systemd --user manager's `Groups:` line; a readable register
  in a fresh login shell is not proof the collector has the group. With linger
  enabled the guidance is a reboot or `loginctl terminate-user <user>` (printed,
  never run). When no manager or group record can be found, the session cannot
  be checked: the installer warns and still enables a successful probe.
- Without a terminal, or when sudo fails, the install prints the shell-quoted
  commands, keeps the rendered rule in the user's state directory and leaves
  both the rule and `gpuRegisters: false` unchanged. The manifest entry stays
  `planned`.
- An existing rule is never taken over from another account. A rule for the
  needed private or shared group is adopted (readable, marked or not); a
  byte-identical 1.0 private-group rule is recorded as `owned`, while a
  `rigscope-gpu` rule is always `adopted`, even when this user install wrote it,
  because other accounts in the group may rely on it. A rule for `rigscope-gpu`
  is adopted by a private-group user, never rewritten, with the `usermod` and
  log-out guidance. A rule for any other group is refused. Uninstall removes an
  owned private-group rule through `sudo rm -f` plus
  `sudo udevadm control --reload`, and only while the marker, digest and group
  still match; an adopted or changed rule is left in place.

The sysusers file names exactly the groups that exist, so enabling the collector
later never references a missing group.

The collector unit sets only the supplementary groups that were installed:

```ini
SupplementaryGroups=rigscope-rapl          # default collector role
SupplementaryGroups=rigscope-rapl rigscope-gpu   # with --gpu-registers
```

The udev rules grant those groups read-only access to exactly the needed sysfs
files:

- `powercap`: top-level `intel-rapl:N` package devices only; `energy_uj` and
  `max_energy_range_uj` become `0440 root:rigscope-rapl`. Subdomains,
  constraint files and power-limit writes are untouched.
- PCI (only with `--gpu-registers`): display functions (`class 0x03xx`) of
  vendor `0x10de` (NVIDIA); `resource0` becomes `0440 root:<group>` for
  junction/VRAM register temperatures. The group is `rigscope-gpu` in system
  scope; a user install uses the user's private primary group when it has one
  and `rigscope-gpu` otherwise.

Hardware failures are never fabricated readings. Missing or unreadable sensors
are reported as unavailable by the collector.

## Unit hardening

System units run as dedicated accounts with no capabilities:

```ini
User=rigscope-hub            # or rigscope-collector
Group=rigscope               # shared, so both can read the 0640 config
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
RestrictNamespaces=yes
LockPersonality=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=
AmbientCapabilities=
UMask=0077
Restart=on-failure
```

`ReadWritePaths` contains only the component's state directory and
`ReadOnlyPaths` the binary. The hub additionally sets `PrivateDevices=yes`; the
collector does not, because GPU telemetry reads real device nodes and configured
sysfs registers through its groups.

User units are deliberately thinner: `WantedBy=default.target`,
`Restart=on-failure`, `UMask=0077`, no `User=`, because they run as the
installing user and rely on the user manager for sandboxing needs.

Every unit starts `rigscope hub --config <config>` or
`rigscope collector --config <config>`. Both runtime subcommands are added in
wave 2; until then they exit with a clear "not available in this build" message.

## Configuration file

One file serves both components. The schema is versioned and strictly parsed:
unknown fields are an error so a typo never silently takes effect.

```json
{
  "version": 1,
  "components": {
    "hub": {
      "enabled": true,
      "listen": "127.0.0.1",
      "port": 8081,
      "origin": "",
      "tlsCert": "",
      "tlsKey": "",
      "forwardedHeaders": "x-forwarded",
      "trustedProxies": []
    },
    "collector": {
      "enabled": true,
      "destinations": [],
      "samplingInterval": "2s",
      "sharing": {
        "cpu": true,
        "memory": true,
        "swap": true,
        "gpus": true,
        "disks": true,
        "network": true,
        "networkAddresses": false,
        "processes": true,
        "processArguments": false
      },
      "hardware": {
        "gpuRegisters": false
      }
    }
  }
}
```

| Key | Meaning |
| --- | --- |
| `version` | Schema version. Currently `1`. |
| `components.hub.enabled` | Run the dashboard for this installation. |
| `components.hub.listen` | Bind address (`127.0.0.1`, a LAN address, `0.0.0.0`). |
| `components.hub.port` | TCP port, 1–65535. |
| `components.hub.origin` | Public root URL for links, cookies and origin checks. No path, query, fragment, credentials or trailing slash. Required and canonical when `listen` is not a loopback address. |
| `components.hub.tlsCert`, `tlsKey` | Optional PEM paths; both or neither. |
| `components.hub.forwardedHeaders` | The one forwarded-header family trusted from `trustedProxies`: `x-forwarded` (`X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Host`) or `forwarded` (RFC 7239 `Forwarded`). Defaults to `x-forwarded`. The other family is ignored and stripped. |
| `components.hub.trustedProxies` | IP addresses or CIDR ranges allowed to supply forwarded headers. Plaintext non-loopback listening is refused without either TLS or this list, and a listener admitted by this list accepts TCP connections only from these addresses or from loopback. |
| `components.collector.enabled` | Collect locally and deliver to the destination. |
| `components.collector.destinations` | List of `{ "url": "...", "caFile": "..." }`. `caFile` optionally pins a private CA for an `https`/`wss` destination. 1.0 accepts at most one entry; the list keeps multi-hub possible later. Destinations never contain a grant or credential. |
| `components.collector.samplingInterval` | Go duration string from `100ms` to `1h`. |
| `components.collector.sharing` | Collector-local privacy policy, matching the 0.2 prototype names: `cpu`, `memory`, `swap`, `gpus`, `disks`, `network`, `networkAddresses`, `processes`, `processArguments`. Metric families default on, `networkAddresses` and `processArguments` default off. |
| `components.collector.hardware.gpuRegisters` | Opt-in PCI BAR0 register temperatures. Default `false`; `install --gpu-registers` installs the udev rule and records `true` once a register read succeeds. User scope applies only the root-owned rule through `sudo`; see [Installation](../installation.md#gpu-register-temperatures). |

Defaults are both components enabled, hub on `127.0.0.1:8081`, no destination,
a `2s` sampling interval and the privacy-preserving sharing policy. `install
--role hub|collector|both` selects what a fresh install enables. Re-running
`install` repairs files and services and may add a component, but never
silently disables one that is already enabled.

`install --hub URL --token TOKEN [--hub-ca FILE]` normalizes the hub address
(`https://host` becomes `wss://host/api/collector/connect`; plaintext is
allowed only on loopback), records it and the optional pinned CA in the
configuration, and writes the one-time grant to
`<stateRoot>/collector/enrollment.json` with mode `0600`. On its next start the
collector enrolls with that grant, saves the issued credential to
`<stateRoot>/collector/credentials.json` and deletes the grant. When the
collector is already enabled and running, `install` restarts it after staging
the grant so enrollment happens immediately.

A fresh `--role both` install, and a later `--role both` that enables the
collector on a hub-only machine, pair the local collector automatically instead
of requiring `--hub`/`--token`. Install records the hub's loopback WebSocket
endpoint (or, when the hub terminates TLS with a certificate that covers a
loopback address, `wss://` plus that certificate as the pinned destination CA;
otherwise the canonical `origin` host). It writes a one-hour grant to the
collector's `enrollment.json` and a hub-owned
`<hubStateDir>/collector-pairing.json` bootstrap file containing only the
grant's SHA-256 hash, expiry and intended use (`colocated-pairing`). The file is
mode `0600`, owned by `rigscope-hub:rigscope` in system scope, is not part of
the installation manifest, and is deleted after the hub imports the hash into
the registry before it starts listening. Automatic pairing creates no grant when
the collector already has a destination or a credential for the local hub, so
re-running `install` on an enrolled machine is a no-op.

### Validation

Configuration is validated before it is written and before any runtime uses it:

- a hub port outside 1–65535, an empty listen address or whitespace is refused;
- an origin must be an absolute, canonical `http`/`https` root URL with a
  non-empty hostname, a valid port and no path, query, fragment, credentials
  or trailing slash; a non-loopback `listen` address requires one. The
  canonical form lowercases the host and removes the scheme's default port, so
  `https://Hub.Example.com:443/` is stored as `https://hub.example.com` and
  `https://:443` is refused;
- `tlsCert` and `tlsKey` must be set together;
- trusted proxies must be IP addresses or CIDR ranges;
- `forwardedHeaders` must be `x-forwarded` or `forwarded` when set.
- at most one destination, with an `http`, `https`, `ws` or `wss` URL and a
  host, no embedded credentials and no fragment;
- plaintext `http`/`ws` destinations are accepted only on loopback; anything
  else must be `https`/`wss`;
- `caFile` must be an absolute path and only accompanies an `https`/`wss`
  destination;
- sampling must be at least `100ms` and at most `1h`.

A disabled hub's network settings are not validated. A disabled collector's
sampling interval and any recorded destination are still validated, but a
hub-only machine needs no collector destination. An enabled collector with no
destination is explicitly "awaiting enrollment", not an error; when a
destination is recorded, exactly one is required.

The `sharing` and `hardware` sections are required in a version 1 file.
Omitting them is an error rather than silently interpreting every flag as
`false`.

Writes are atomic: a temporary file in the same directory, `fsync`, then
`rename`. Readers never observe a partial file. User configuration is mode
`0600`; system configuration is `0640 root:rigscope`.

### Reverse-proxy contract

Forwarded headers are evidence about the client and transport only when they
come from an address in `trustedProxies`. The proxy must:

- connect from one of those addresses (the hub closes any other non-loopback
  TCP peer on a plaintext listener admitted by this list). Loopback peers are
  always admitted so local readiness probes and operator requests keep working,
  but admission never makes loopback a trusted forwarded-header source;
- overwrite, not append to, `X-Forwarded-Proto` and `X-Forwarded-Host` with a
  single value (`https` and the canonical public host), and write exactly one
  of those two values per request;
- append the address it saw to `X-Forwarded-For` (or emit one RFC 7239
  `Forwarded` element per hop with `for=`, `proto=` and `host=`), and never
  forward a client-supplied value without appending;
- use only the family selected by `forwardedHeaders`; the hub ignores and
  strips the other family entirely;
- forward origin-form request targets. The hub rejects absolute-form targets
  and CONNECT with 400, so the client authority can never be promoted into the
  request's Host.

`Forwarded` is parsed strictly against RFC 7239 (tokens and quoted-strings with
escaped pairs, `;`-separated pairs, `,`-separated elements). A single syntax
error — an unterminated quoted-string, a bad escape, an empty pair, or a
duplicate parameter in one element — discards every forwarded fact for that
request: the hub falls back to the TCP peer, records one debug log line, and
never tries to recover a partial chain. (This is what stops a client prefix
such as `for=198.51.100.66;ext="` from swallowing the element an appending proxy
adds behind it.) Walking the chain from the right, an element without a usable
`for` is a trust barrier: the client address becomes the TCP peer. Proto and
host are taken from the nearest element contributed by a trusted hop, so a
two-hop chain that carries `proto=https` on both elements still reports HTTPS
and an attacker-supplied farther element cannot override it.

Each response write of up to 64 KiB renews an idle-write deadline (30 s by
default). A large download through `io.Copy` therefore takes as long as it
needs while it keeps making progress, and a client that stops reading is cut
off at the next stalled write.

A remote proxy therefore needs a protected backend link (a private network, a
loopback tunnel, or TLS). The hub cannot authenticate a plaintext peer by
address alone across an untrusted network.

## Forward migrations

`internal/config` owns an ordered migration chain:

```go
type Migration struct {
    From  int
    To    int
    Apply func(map[string]any) error
}
```

Loading reads `version` first, then for an older file migrates a generic
`map[string]any` through the chain (so keys can be renamed and value types
changed) and only strict-decodes the final object into the current schema.
Unknown fields, including leftovers a migration forgot to rename, fail at that
last step. Rules:

- A file **newer** than the binary supports is refused with a message to
  upgrade Rigscope first. It is never downgraded or partially read.
- A missing migration step is an error; migrations are never skipped.
- Duplicate or non-sequential entries are rejected before anything is applied.
- Migrations are deterministic, take no environment input and never discard
  user data.
- Any change to the schema bumps `version` and appends a migration, so an older
  1.x binary refuses a newer file instead of silently dropping fields.
- Future releases append to `DefaultMigrations()` and bump `CurrentVersion`.

1.0 has no real migrations yet; the framework is tested with test-only
`0 → 1` migrations that rename a key and change a value from string to number.
0.2 configuration is not migrated: there is no importer.

## 0.2 coexistence

0.2 user units are named `rigscope-<12 hex hash>-hub.service`,
`rigscope-<12 hex>-collector.service`, plus an older combined
`rigscope-<12 hex>.service`. 1.0 uses stable names
`rigscope-hub.service` and `rigscope-collector.service`, which cannot collide
with the hashed pattern.

0.2 system units reused the stable `rigscope-hub.service` and
`rigscope-collector.service` names. They are recognised by their markers
(`# Managed by \`rigscope system\`` / `# Managed by Rigscope system
installation`) and by an `/opt/rigscope` ExecStart, never by the 1.x marker.

Before writing anything, `install` scans both scopes and refuses if it finds:

- hashed user units, or the older combined `rigscope-<12 hex>.service`;
- 0.2 system units, the combined `rigscope.service`, or `/opt/rigscope`;
- 0.2 user state (`config.json`, `release-state.json`, `runtime/`,
  `runtime-previous/`, `reset-pending.json`, `hub/enrollment.json`,
  `collector/state.json`);
- 0.2 system state (`system-install.json`, `power-access.json`,
  `/etc/rigscope/hub.env`, `/etc/rigscope/collector.env`,
  `/etc/sysusers.d/rigscope-rapl.conf`).

The refusal names the exact files:

```text
a Rigscope 0.2 installation is present (/home/alice/.config/systemd/user/rigscope-6629b1795b8f-hub.service, ...)

Rigscope 1.0 does not import 0.2 data. Remove the old installation first:
  rigscope uninstall          # part of 0.2, or: npx --yes rigscope@latest uninstall
Then run this installer again
```

The user runs 0.2's own `uninstall` first, then installs 1.0 fresh. No files,
configuration or history are read from 0.2. `uninstall` applies the same
detection: without a valid 1.x manifest it refuses and never deletes anything,
even when the default state directory looks exactly like 0.2's.

## Upgrade and rollback guarantees for 1.x

These are the promises 1.x releases must keep:

1. **No data importer is needed after 1.0.** Upgrades preserve accounts,
   history, collector identities, configuration and issued credentials in
   place. Layout paths and file names above do not change within 1.x.
2. **Config migrations are forward-only.** A 1.x binary refuses a config from a
   newer 1.x rather than discarding it. Downgrades that would cross a schema
   version are not supported; use the retained binary only with its paired
   data.
3. **Atomic replace.** `upgrade` downloads
   `rigscope_<version>_linux_<goarch>.tar.gz` and `checksums.txt` from
   <https://github.com/Triet1402/rigscope/releases>, verifies SHA-256, extracts
   only the `rigscope` binary (bounded size, no path traversal) and replaces the
   installed binary through a same-directory temp file + `fsync` + `rename`.
   The previous inode is retained as `rigscope.prev`.
4. **Health check and automatic rollback.** After replacing the binary, enabled
   units are restarted. The hub must answer `GET /api/health`; every enabled
   unit must report active. If either check fails within the bounded retry
   window, the binary is swapped back automatically, units are restarted and
   the command exits nonzero.
5. **Manual rollback.** `rigscope rollback` swaps `rigscope.prev` back, restarts
   enabled units and runs the same health check. A rollback that cannot quiesce,
   restore data or the previous binary, or pass the restored health check stops
   in `failed-needs-attention` with the snapshot and binaries kept for an
   operator instead of discarding an unknown state.
6. **Idempotent install.** `install` may be re-run at any time to repair the
   binary, units, enablement and startup settings. It keeps existing
   configuration and credentials and never disables an enabled component.
   Before any mutation it preflights every destination: an existing file is
   overwritable only when the 1.x manifest records it as owned. A file with the
   1.x marker but no manifest is treated as foreign, never adopted. New files
   are created with no-overwrite semantics (`O_CREATE|O_EXCL` or link-then-
   rename) and promoted to owned only afterwards. On a system install
   `systemd-sysusers` runs before the first `chown`, so a fresh machine can
   create the accounts and retry after an interruption at any step.
7. **Versioned protocol, not lockstep upgrades.** Collectors and hubs carry a
   protocol version; the hub accepts the current and previous protocol. An
   upgrade does not require stopping the other component.

Health retry policy: 20 attempts at 500 ms intervals (`/api/health` plus
`systemctl is-active` for each enabled unit).

`upgrade --version X` selects an exact release; without it the latest published
release is used. A development binary reports `dev`, so only release builds
short-circuit with "already installed".

## Uninstall

`rigscope uninstall` requires a valid `<stateRoot>/install-manifest.json`
(marker `rigscope-installation/1`, matching scope, version, and only paths
inside the documented layout). Without it, nothing is created, stopped or
deleted; when 0.2 artifacts are present the command prints 0.2's uninstall
instructions instead.

With a valid manifest it stops and disables every recorded unit name,
including a unit whose file was deleted but which systemd still has loaded,
verifies each one is inactive, and aborts before deleting anything if any stop,
disable or verification fails. Only a unit the manager reports as unknown and
inactive is skipped. It then removes only owned paths: unit files and
integration files that still carry the 1.x marker and whose identity matches
the manifest, the binary and `rigscope.prev`, Rigscope transaction temp files
beside the binary, and — by default — the configuration file and state
directory (including `upgrade-journal.json`, `staging/` and `backups/`).
Integration files that the manifest does not record are left alone, even if
they look like Rigscope files.

Every deletion is relative to a directory descriptor opened component by
component with `O_DIRECTORY|O_NOFOLLOW`. The parent's recorded device, inode
and owner are verified first, and recursive state removal never follows a
symlink. A parent replaced by a symlink — before uninstall or during the stop
phase — makes uninstall abort with nothing deleted. The binary, `config.json`
and the enrollment grant may legitimately change inode during an upgrade; their
parent and file type are still verified, but a changed inode does not block
deletion. Unit and integration files keep a strict identity and SHA-256 check.

Recursive removal refuses before deleting anything when the tree contains a
mount point recorded in `/proc/self/mountinfo`, including a same-device bind
mount that `st_dev` cannot distinguish. Traversal also refuses to descend into
a directory on another device, and a pre-pass refuses a tree deeper than 128
levels so one deletion cannot exhaust the process descriptor table.

System scope also reloads udev after removing a rule (`udevadm control
--reload`, through the injected command runner) and reports a failure instead
of silently leaving the old grants active. Accounts and groups are left in
place; removing system users is left to the administrator. Removing a udev rule
does not undo group/mode changes already applied to existing sysfs nodes; a
reboot (or the next device creation) restores the kernel defaults.

`rigscope uninstall --keep-data` keeps `config.json` and the state directory so
a later install can reuse accounts, history and collector identity. It rewrites
the manifest to claim only the kept data, so a later uninstall cannot delete a
foreign file that appeared at a removed path.

## Installation lock

Install, uninstall, start, stop and restart take a non-blocking flock on
`$XDG_RUNTIME_DIR/rigscope/install.lock` in user scope, or
`/run/rigscope/install.lock` in system scope. When `XDG_RUNTIME_DIR` is unset,
user scope falls back to `/tmp/rigscope-<uid>/install.lock`; the directory must
be owned by the invoking user and mode `0700` (and the lock file itself is
opened with `O_NOFOLLOW`), so a local attacker cannot pre-create it. The lock
is deliberately outside the state directory: uninstall deletes the state
directory while holding the flock, and removing a locked file would make a
concurrent command create a second inode with its own lock.

A concurrent command fails immediately with "another rigscope operation is
running" rather than racing a half-written installation.
`internal/platform/lock` also exposes a timeout variant for the upgrade
activation transaction. The lock file is never removed; the kernel releases
the lock when the process exits.

In-process re-entry is scoped to an explicit owner token: recovery, the
upgrade transaction and rollback share one token, while any other caller is a
separate operation and cannot borrow the held lock. A child process never skips
acquisition on a flag. The parent passes its locked descriptor in
`cmd.ExtraFiles` and names it in `RIGSCOPE_INSTALL_LOCK_FD`; the child checks
that the descriptor's device and inode match the lock path and re-locks the
same open file description. `rigscope migrate --system` still enforces root
authorization independently of the inherited descriptor.

## Upgrade journal and backups

`upgrade` is one transaction recorded in `<stateRoot>/upgrade-journal.json`
(mode `0600`). Format 2 is self-contained: it persists the resolved scope and
every layout path (binary, `rigscope.prev`, config, state, collector identity,
hub database and the journal itself), the enabled unit names and their start
order, and the hub readiness settings. Recovery reads these from the journal
before loading `config.json`, so a migrated, partially written or invalid
configuration cannot block recovery. The journal also carries the previous and
new schema versions, `compatibleDown`, the SHA-256 of both binary identities,
the prepared-binary path and phase flags (`servicesStopped`, `backupComplete`,
`publishStarted`, `acceptedWrites`).

The success path is `locked → staged → quiesced → backed-up → migrated →
prepared → activated → verified → done`. `acceptedWrites` is durably written
before the first start of the new release; after that point recovery never
restores the pre-upgrade data snapshot. A failure or interruption rolls back
through explicit substates: `rolling-back → rollback-quiesced →
rollback-data-restored → rollback-binary-restored → rollback-writers-started →
rolled-back`. `rolled-back` is written only after restoration and the restored
readiness contract succeed; any restore or publish failure stops in
`failed-needs-attention` with all artifacts kept. Once
`rollback-writers-started` is recorded, the snapshot is never replayed.

The new binary is prepared as `<binary>.<transaction>.new` beside the target,
fsynced and hashed before the journal records `prepared`; the manifest records
ownership of that temp file. The current binary is durably retained as
`<binary>.<transaction>.prev`, fsynced, hashed and renamed to `rigscope.prev`
before the prepared file replaces the target. Recovery acts only on transaction
temp files whose path and SHA-256 match the journal; a mismatched file is
discarded and a missing `rigscope.prev` is an error, never "no swap needed".

Rollback recovery verifies both `.restore-new` and `.restore-old` candidates
against the journal digests before either rename. A mismatched candidate is
preserved untouched, the currently installed binary is kept, and the journal
records `failed-needs-attention`. Staging and transaction temp files are
removed on a terminal outcome, and their manifest entries are forgotten.

Because recovery may have restored a different binary than the process running
`upgrade`, `upgrade` asks the installed binary for its version (bounded to ten
seconds) and uses that as the transaction's `fromVersion`, falling back to the
journal's recorded version. If the restored file differs from the running
executable, `upgrade` re-executes the installed binary to continue instead of
comparing the running process's build version.

`backups/<name>/` contains `manifest.json` (format 2), `config.json`, the
collector identity tree and the database set as `hub.db`, `hub.db-wal` and
`hub.db-shm`. Every copied file and every created directory (and its parents up
to an existing ancestor) is fsynced before `backed-up` is journaled. Restore
puts the set back exactly: sidecars that existed are restored and sidecars that
did not exist in the snapshot are removed. Retention keeps the newest three
completed backups plus any snapshot a non-terminal journal or a supported
rollback still needs; older directories are removed without following
symlinks.

Upgrade, rollback, recovery and `migrate` use the same installation lock as
`install` and `uninstall`; the staged binary inherits the parent's locked
descriptor instead of deadlocking behind the transaction that is running it.
Migration commands have a configurable deadline (default five minutes) and
version probes a ten-second deadline; both run in their own process group with
bounded stdout/stderr and a bounded wait delay. Readiness runs the installed
binary's `collector --check` (ten-second deadline) with the persisted
configuration and state paths; in system scope it drops to the collector
service account when this process is root, and records a note when it cannot.
`--check` opens the collector lease for writing, so the permission problem that
would stop the service fails readiness.

## Implementation map

| Concern | Package |
| --- | --- |
| Schema, defaults, validation, migrations, paths | `internal/config` |
| Unit rendering, accounts/udev content, systemd Manager | `internal/platform/systemd` |
| Atomic writes and binary replacement | `internal/platform/fsutil` |
| Installation flock | `internal/platform/lock` |
| Commands, ownership manifest, preflight | `internal/cli` |
| Subcommand dispatch | `cmd/rigscope` |
| Bootstrap downloader | `scripts/install.sh` |
| Golden unit files | `testdata/units/` |

## Not in this build

- `rigscope hub` and `rigscope collector` runtimes (dispatch point exists).
- Collector enrollment against `--hub`/`--token`; install only stages the
  private grant and destination.
- Hardware probes in `doctor` (stubbed with TODO checks).
- Hub database, UI and protocol.
