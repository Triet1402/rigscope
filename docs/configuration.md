# Configuration reference

Rigscope 1.0 uses one versioned JSON file for both components:
`~/.config/rigscope/config.json` in user scope, or `/etc/rigscope/config.json`
in system scope. The file is the only way to configure a component: there is no
`HUB_*`/`COLLECTOR_*` or `RIGSCOPE_*` settings interface.

You rarely need to edit it. `rigscope configure --show` prints the current
settings, and `rigscope configure` changes them safely: components, port,
origin, listen address, TLS, proxy trust, history storage, sharing, sampling
interval and GPU register sampling. It previews the change, checks it, and asks
before restarting a service. See
[Installation](installation.md#change-settings) for examples. Edit the file by
hand only for the few settings without a flag, then run `rigscope restart`.

Start with [sharing policy](#sharing-policy) to control what leaves a machine,
[TLS and proxies](#tls-and-trusted-proxies) for remote access, or
[telemetry mode](#hub-telemetry-mode) to keep history across restarts.

<details>
<summary>Advanced: internal environment variables and installer overrides</summary>

A few `RIGSCOPE_*` variables exist for internal plumbing and are not a
configuration surface. The binary reads:

- `RIGSCOPE_HUB_PORT`, `RIGSCOPE_HUB_DATABASE_PATH`, `RIGSCOPE_HUB_FLUSH_MS`,
  `RIGSCOPE_HUB_DISABLE_BOOTSTRAP` — test/fixture overrides for the hub
  process. Do not set them in production; the JSON file is authoritative.
- `RIGSCOPE_RELEASE_BASE_URL` — loopback-only test seam for the release
  downloader.
- `RIGSCOPE_UPGRADE_REEXEC` and `RIGSCOPE_INSTALL_LOCK_FD` — internal
  child-process handshakes for `upgrade` and the installation lock. Never set
  them yourself.

The installer script additionally honours `RIGSCOPE_VERSION` (pin a release)
and the undocumented test hook `RIGSCOPE_INSTALL_BASE_URL`.

The file is strictly parsed. Unknown fields are an error, so a typo never
silently takes effect. The schema version is explicit and migrated forward, so a
1.x upgrade keeps your settings and an older binary refuses a newer file instead
of dropping fields. Rigscope 0.2 configuration is not migrated and no importer
exists; a 1.0 installation starts from the defaults below.

`rigscope install` writes the file; `install`, `upgrade` and `uninstall` treat it
as part of the installation. You may edit it by hand, then restart the affected
components:

```sh
rigscope restart
```

There is no live reload. Changes take effect when the process next starts, and
an invalid file prevents that component from starting.

`rigscope configure` changes these settings for you: it validates the result,
rebinds a locally paired collector to a new hub address and restarts only what
it changed after asking once.

</details>

## A complete default file

A fresh installation writes this shape. The whitespace may differ; the keys and
values do not, with one exception: a default `--role both` install pairs the
local collector with the local hub automatically, so
`components.collector.destinations` contains one loopback entry:

```json
"destinations": [
  { "url": "ws://127.0.0.1:8081/api/collector/connect" }
]
```

A hub-only or collector-only install leaves `destinations` empty until an
explicit `install --role collector --hub URL --token TOKEN`, which records the
destination and enrolls. `rigscope collector enroll` only stores a credential
for a URL; it never adds a destination, so a collector whose `destinations` is
empty stays idle after it. The example below shows the empty form.

```json
{
  "version": 1,
  "components": {
    "hub": {
      "enabled": true,
      "telemetryMode": "memory",
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

A collector-only install has `components.hub.enabled` set to `false`. A
hub-only install has `components.collector.enabled` set to `false`.

## Top level

| Key | Type | Default | Rules |
| --- | --- | --- | --- |
| `version` | integer | `1` | Required. Must equal the schema this binary understands. A newer version is refused with "upgrade rigscope first"; it is never downgraded or partially read. |
| `components` | object | — | Required. Holds `hub` and `collector`. |

## `components.hub`

| Key | Type | Default | Meaning and validation |
| --- | --- | --- | --- |
| `enabled` | boolean | `true` | Run the dashboard. When false, the rest of the hub section is not validated and the hub unit is disabled. Change with `rigscope configure --hub on\|off`. |
| `listen` | string | `"127.0.0.1"` | Bind address: a loopback address, a LAN address, or `0.0.0.0`. Required and non-blank when enabled; whitespace is rejected. Change with `--listen ADDRESS`. |
| `port` | integer | `8081` | TCP port, 1–65535. |
| `telemetryMode` | string | `"memory"` | `memory` or `persistent`. Change with `rigscope configure --telemetry-mode MODE`; see [Hub telemetry mode](#hub-telemetry-mode). |
| `origin` | string | `""` | Canonical public root URL for links, cookies and Origin checks. Required when `listen` is not a loopback address. See [Canonical origin](#canonical-origin). |
| `tlsCert` | string | `""` | Optional PEM certificate path. Must be set with `tlsKey`. Set both with `--tls-cert FILE --tls-key FILE`; clear both with `--no-tls`. |
| `tlsKey` | string | `""` | Optional PEM private-key path. Must be set with `tlsCert`. |
| `forwardedHeaders` | string | `"x-forwarded"` | The single forwarded-header family trusted from `trustedProxies`: `"x-forwarded"` (`X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Host`) or `"forwarded"` (RFC 7239 `Forwarded`). The other family is ignored and stripped. Change with `--forwarded-headers`. |
| `trustedProxies` | array of strings | `[]` | IP addresses or CIDR ranges allowed to supply forwarded headers. Plaintext non-loopback listening is refused without either TLS or this list. |

When the hub is disabled, a hub-only machine does not need an origin, TLS or
trusted proxies. When it is enabled and listens beyond loopback, it needs TLS or
at least one trusted proxy; the hub refuses to start otherwise.

A loopback address in `trustedProxies` makes a reverse proxy on this machine
indistinguishable from the local collector, so the hub refuses every colocated
pairing redemption. Automatic pairing therefore declines while such a proxy is
configured; use an enrollment token instead.

## `components.collector`

| Key | Type | Default | Meaning and validation |
| --- | --- | --- | --- |
| `enabled` | boolean | `true` | Collect locally and deliver to the destination. Change with `rigscope configure --collector on\|off`. |
| `destinations` | array | `[]` | At most one entry in 1.0: `{"url": "...", "caFile": "..."}`. An enabled collector with no entry is "awaiting enrollment", not an error. |
| `samplingInterval` | duration string | `"2s"` | Go duration from `100ms` to `1h`. Rates are derived from monotonic time, not wall-clock deltas. Change with `--sampling-interval 5s`; shorter intervals cost more CPU here and more hub storage. |
| `synthetic` | boolean | `false` | Optional testing/demo mode: emit clearly fake, explicitly synthetic readings and read no host resources. Never enable it for real monitoring. It is omitted from the file when false. |
| `sharing` | object | privacy-preserving defaults | Required; every key below must be present. It decides what leaves the machine. |
| `hardware` | object | `{"gpuRegisters": false}` | Required. Opt-in hardware access. |

### Destinations

One collector talks to one hub. The list shape is kept so multi-hub delivery can
return in a later release, but a second entry is refused.

```json
"destinations": [
  {
    "url": "wss://hub.example.com/api/collector/connect",
    "caFile": "/etc/rigscope/hub-ca.pem"
  }
]
```

- `url` is the hub's collector WebSocket endpoint: `wss://` (or `ws://` on
  literal loopback, `127.0.0.0/8` or `::1`) with the path exactly
  `/api/collector/connect`, a host, and no credentials, query or fragment.
  `rigscope install --hub https://host` writes this form for you; when editing
  the file by hand, use the full endpoint.
- `caFile` must be an absolute path and is allowed only for `wss`. It
  replaces the system trust store for that destination, so only a certificate
  issued by that bundle is accepted.
- Destinations never contain an enrollment grant or issued credential. Those
  live in the collector's private state directory.

`install --hub URL --token TOKEN [--hub-ca FILE]` records the destination and
writes the one-time grant to `<stateRoot>/collector/enrollment.json` (0600). The
collector consumes the grant at startup, enrolls, and stores the issued
credential in `<stateRoot>/collector/credentials.json`. `rigscope collector
enroll` performs the same exchange explicitly.

### Sharing policy

The collector applies this policy locally, before any metrics, inventory or
process detail is queued for transmission. It is not a hub-side filter: withheld
data never leaves the machine.

| Key | Default | What it shares |
| --- | --- | --- |
| `cpu` | `true` | CPU usage, per-core usage, CPU temperature, package power, I/O wait and PSI CPU pressure. |
| `memory` | `true` | Memory used/total and memory PSI pressure. |
| `swap` | `true` | Swap used/total. |
| `gpus` | `true` | GPU identifiers, utilization, memory, temperature, power and, when enabled, junction/VRAM temperatures. |
| `disks` | `true` | Filesystem usage and inode use, disk throughput and latency, mountpoints and device names, disk PSI pressure. |
| `network` | `true` | Interface names and throughput. |
| `networkAddresses` | `false` | Interface IP addresses and other host-identifying addresses. Also scrubs addresses from failure reasons and resource IDs when off. |
| `processes` | `true` | Whether process detail can be requested at all. |
| `processArguments` | `false` | Full command lines. When off, process detail omits the command; the environment is never collected either way. |

Change it on the machine itself with `rigscope configure --share FAMILY=on|off`
(repeat the flag, or separate several with commas). The flag names are `cpu`,
`memory`, `swap`, `gpus`, `disks`, `network`, `network-addresses`, `processes`
and `process-arguments`. Turning on `network-addresses` or `process-arguments`
shows what becomes visible and always asks first. A hub has no way to change
this policy.

Every key is required by the configuration validator. A fresh file and the
collector's built-in policy both default `processes` to `true`; a hand-written
file must still include the key explicitly.

When a metric family is withheld, the collector sends the family as unavailable
with the reason `withheld by local sharing policy` rather than omitting it or
sending zero. Inventory families keep their capability entry but report
`withheld` and list no resources. Withholding `processes` makes process detail
report `withheld` too.

### Hardware

| Key | Default | Meaning |
| --- | --- | --- |
| `gpuRegisters` | `false` | Read NVIDIA PCI BAR0 thermal registers for junction and VRAM temperatures. |

`install --gpu-registers` installs the `90-rigscope-gpu-temps.rules` udev rule
and records `true` once a register read succeeds. A system install also creates
the `rigscope-gpu` group and adds it to the collector unit; a user install uses
the user's private primary group when it has one. A system collector install
also provisions the RAPL rule and `rigscope-rapl` group. A user install leaves
RAPL permissions to the operator; a hub-only install provisions neither
hardware group nor rule.

Both scopes support register temperatures now. A system install writes the rule
as root and uses the shared `rigscope-gpu` group. A user install runs as you and
applies only the root-owned udev rule through `sudo`: when your primary group is
private the rule grants that group and works immediately, otherwise it uses
`rigscope-gpu` and needs a full log-out before `rigscope install
--gpu-registers` can verify and enable it. The private group is only chosen when
`/etc/nsswitch.conf` serves `passwd` and `group` from local files and your uid is
in `/etc/passwd`; NSS-backed accounts use the shared group, and so does a
cgo-free build whose uid is missing from `/etc/passwd` (the name then comes from
`USER` or `LOGNAME`). For the shared group the installer also checks the running
systemd --user manager's supplementary groups (including the linger case) and
keeps `gpuRegisters` off when that manager is known to lack the rule's group. If
the manager cannot be found or the group cannot be resolved, the session cannot
be checked: the installer warns and still enables sampling when the register
probe succeeds. A rule for `rigscope-gpu` is recorded as adopted even when this
user install wrote it, and a rule for another group is never taken over. An
installation counts as its own only a rule for the user's private group, with
the digest and group recorded in the manifest; uninstall deletes a rule only
while all of that still matches. See
[Installation](installation.md#gpu-register-temperatures) for the commands and
the no-terminal path.

`rigscope configure --gpu-registers off` stops register sampling without
removing the rule or group access; `install --gpu-registers` turns it back on,
and uninstall removes the access.

Register temperatures are undocumented and card-specific. When access is
missing or a word does not decode, the reading is unavailable, not zero.

## Validation

Configuration is validated before it is written and before any runtime uses it.

- An empty or whitespace-containing `listen` is refused when the hub is enabled.
- A hub port outside 1–65535 is refused.
- An origin must be an absolute, canonical `http`/`https` root URL with a
  non-empty hostname, a valid port, and no path, query, fragment, credentials or
  trailing slash. A non-loopback `listen` requires one.
- `tlsCert` and `tlsKey` must be both set or both empty.
- `forwardedHeaders` must be `"x-forwarded"` or `"forwarded"` when set.
- Every trusted proxy must be an IP address or CIDR range.
- At most one destination; its URL rules are above.
- `samplingInterval` must be at least `100ms` and at most `1h`.
- `sharing` and `hardware` must be present, and every sharing key and
  `hardware.gpuRegisters` must be present, rather than defaulting missing values
  to `false`.
- A `version` newer than the binary is refused. A version that should have been
  migrated but is not is also refused.

A disabled hub's network settings are not validated, so a collector-only machine
does not need an origin. Collector settings are validated regardless of the
collector's enabled state because the same file is read by both components.

Writes are atomic: a temporary file in the same directory, `fsync`, then
`rename`. Readers never observe a partial file. The user file is mode 0600; the
system file is 0640 `root:rigscope`.

## Canonical origin

`origin` is the public root URL the browser uses. It is a static trust input:
Rigscope never derives it from a request header.

Rules:

- `http` or `https`, with a hostname. `https://:443` is refused.
- No path, query, fragment, credentials or trailing slash.
- The host is lowercased and the scheme's default port is removed.

Examples:

| Written | Stored / accepted as |
| --- | --- |
| `https://Hub.Example.com:443/` | `https://hub.example.com` |
| `http://hub.example.com:80` | `http://hub.example.com` |
| `https://hub.example.com:8443` | `https://hub.example.com:8443` |
| `https://hub.example.com/dashboard` | refused (path) |
| `https://user@hub.example.com` | refused (credentials) |

The origin is used for dashboard links, cookie selection and exact Origin
checks. In remote mode every request must carry the canonical `Host`; a missing
or duplicate Host is 400, a mismatch is 421, and a present `Origin` that is not
byte-identical to the configured origin is 403. Mutating requests additionally
require that exact Origin.

## TLS and trusted proxies

### Direct TLS

Set both `tlsCert` and `tlsKey` to PEM files readable by the service account,
most easily with `rigscope configure --tls-cert FILE --tls-key FILE`, which
checks the pair, its expiry and names, and service-account access first.
The hub serves HTTPS directly, negotiates TLS 1.2 or newer with ECDHE AEAD
suites, and sends `Strict-Transport-Security: max-age=31536000`. HSTS is also
sent when the configured origin is HTTPS behind a trusted proxy; it is never
sent for a plaintext public origin. HSTS does not include subdomains or preload.

Rigscope does not obtain, renew or reload certificates. Restart the hub after
replacing them. A bad pair fails startup before the hub accepts traffic.

### Plaintext behind a proxy

A non-loopback plaintext listener is refused unless `trustedProxies` is
non-empty. Such a listener accepts TCP connections only from those addresses and
from loopback; every other peer is closed before request parsing. Loopback
remains admitted for local health probes and operator requests, but **loopback
is not a trusted forwarded-header source**. Admission and header trust are
separate.

Forwarded headers are believed only when the TCP peer is in `trustedProxies`.
Exactly one family is authoritative; the other is ignored and stripped.

| `forwardedHeaders` | Headers read |
| --- | --- |
| `x-forwarded` (default) | `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Host` |
| `forwarded` | RFC 7239 `Forwarded` |

For `X-Forwarded-For`, Rigscope walks the chain from the right, skipping trusted
proxy addresses, and takes the first untrusted address as the client. A
malformed hop or a chain made only of trusted addresses falls back to the TCP
peer; the leftmost client-supplied value is never trusted blindly. `Forwarded`
is parsed strictly against RFC 7239. A single syntax error discards every
forwarded fact for that request and logs one debug line; Rigscope never tries to
recover a partial chain. This prevents a crafted client prefix from swallowing
the element an appending proxy adds behind it.

`X-Forwarded-Host` is available as metadata, but Host validation always uses the
request's `Host` header. A proxy must therefore send the canonical public host
as `Host`, not merely in a forwarded header.

### The reverse-proxy contract

The proxy is responsible for these facts. If it does not follow them, Rigscope
will either reject the request or record the wrong client and scheme.

- Connect from an address listed in `trustedProxies`. Loopback peers are
  admitted to the listener but do not become trusted header sources.
- Overwrite, not append to, `X-Forwarded-Proto` and `X-Forwarded-Host` with a
  single value: `https` and the canonical public host. Emit exactly one of those
  values per request.
- Append the address it saw to `X-Forwarded-For` (or emit one RFC 7239
  `Forwarded` element per hop with `for=`, `proto=` and `host=`). Never forward a
  client-supplied value without appending.
- Use only the family selected by `forwardedHeaders`. The hub ignores and strips
  the other family entirely.
- Forward origin-form request targets. Rigscope rejects absolute-form targets
  and `CONNECT` with 400, so a client authority can never be promoted into the
  request's Host.
- Preserve the canonical `Host` header.
- Forward WebSocket upgrades for `/api/collector/connect`.
- Stream `/api/live` without buffering, and allow long-lived responses. Responses
  renew an idle-write deadline (30 s) as they make progress; a large download
  takes as long as it needs, and a client that stops reading is cut off.
- Protect the backend link: a private network, a loopback tunnel, or TLS. The hub
  cannot authenticate a plaintext peer by address alone across an untrusted
  network.

Example nginx location fragments:

```nginx
location / {
    proxy_pass http://127.0.0.1:8081;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto https;
    proxy_set_header X-Forwarded-Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;
    proxy_buffering off;
    proxy_read_timeout 1h;
}
```

A matching configuration would set `trustedProxies` to the proxy's loopback or
private address, keep `forwardedHeaders` at `x-forwarded`, set
`listen` to `127.0.0.1`, and set `origin` to `https://hub.example.com`.

## Files, modes and secrets

The layout is computed from the scope. State is private; the installation lock
lives outside the state directory so uninstall can delete the state tree while
holding it.

### User scope

| Path | Mode | Purpose |
| --- | --- | --- |
| `~/.local/bin/rigscope` | 0755 | Current release. |
| `~/.local/bin/rigscope.prev` | 0755 | Retained previous release, after an upgrade. |
| `~/.config/rigscope/config.json` | 0600 | This file. |
| `~/.config/systemd/user/rigscope-hub.service` | 0644 | Hub unit. |
| `~/.config/systemd/user/rigscope-collector.service` | 0644 | Collector unit. |
| `~/.local/state/rigscope/` | 0700 | State root. |
| `.../install-manifest.json` | 0600 | Ownership record; uninstall refuses without it. |
| `.../upgrade-journal.json` | 0600 | Pending upgrade transaction. |
| `.../hub.db`, `hub.db-wal`, `hub.db-shm` | 0600 | Hub SQLite history, accounts, sessions and alert state. |
| `.../hub.lock` | 0600 | Hub process lease; also blocks offline recovery. |
| `.../backups/<name>/` | 0700 | Pre-upgrade database/config/identity snapshots. |
| `.../staging/<version>/rigscope` | 0700 | Verified release binary before activation. |
| `.../collector/` | 0700 | Collector-private directory. |
| `.../collector/collector-runtime.lock` | 0600 | Collector process lease. |
| `.../collector/enrollment.json` | 0600 | Transient one-time grant. |
| `.../collector/credentials.json` | 0600 | Issued collector credential. |
| `.../collector/identity.json` | 0600 | Persistent collector installation ID. |

The installation lock is `$XDG_RUNTIME_DIR/rigscope/install.lock`, or
`/tmp/rigscope-<uid>/install.lock` when `XDG_RUNTIME_DIR` is unset.

### System scope

| Path | Mode and owner | Purpose |
| --- | --- | --- |
| `/usr/local/bin/rigscope` | 0755 root:root | Current release. |
| `/usr/local/bin/rigscope.prev` | 0755 root:root | Previous release. |
| `/etc/rigscope/config.json` | 0640 root:rigscope | This file; both accounts read it through the shared group. |
| `/etc/systemd/system/rigscope-hub.service` | 0644 root:root | Hub unit. |
| `/etc/systemd/system/rigscope-collector.service` | 0644 root:root | Collector unit. |
| `/etc/sysusers.d/rigscope.conf` | 0644 root:root | Service accounts and installed hardware groups. |
| `/etc/udev/rules.d/90-rigscope-rapl.rules` | 0644 root:root | Package energy counters, collector role. |
| `/etc/udev/rules.d/90-rigscope-gpu-temps.rules` | 0644 root:root | GPU registers, only with `--gpu-registers`; also used by user installs, written through `sudo`. |
| `/var/lib/rigscope/` | 0755 root:root | State root (traversable). |
| `/var/lib/rigscope/install-manifest.json` | 0600 root:root | Ownership record. |
| `/var/lib/rigscope/upgrade-journal.json` | 0600 root:root | Pending transaction. |
| `/var/lib/rigscope/backups/` | 0700 root:root | Snapshots. |
| `/var/lib/rigscope/staging/` | 0700 root:root | Staged releases. |
| `/var/lib/rigscope/hub/` | 0700 rigscope-hub:rigscope | Hub private subtree. |
| `/var/lib/rigscope/hub/hub.db` | 0600 rigscope-hub:rigscope | Hub database and WAL set. |
| `/var/lib/rigscope/hub/hub.lock` | 0600 rigscope-hub:rigscope | Hub process lease. |
| `/var/lib/rigscope/collector/` | 0700 rigscope-collector:rigscope | Collector private subtree. |
| `/var/lib/rigscope/collector/enrollment.json` | 0600 rigscope-collector:rigscope | Transient grant. |
| `/var/lib/rigscope/collector/credentials.json` | 0600 rigscope-collector:rigscope | Issued credential. |
| `/var/lib/rigscope/collector/identity.json` | 0600 rigscope-collector:rigscope | Installation ID. |
| `/run/rigscope/install.lock` | 0600 root:root | Installation lock. |

The hub account cannot read the collector's grant or credential, and the
collector account cannot read the hub database. Neither unit writes logs to the
state directory: both use journald with `SyslogIdentifier=rigscope-hub` and
`rigscope-collector`.

The manifest is the proof of ownership. `install` records every path it owns;
`uninstall` deletes only owned paths whose identity still matches. A file that
merely looks like a Rigscope file is never adopted or deleted without that
record.

## Hub telemetry mode

`components.hub.telemetryMode` accepts `"memory"` (the default when omitted) or
`"persistent"`. RAM mode keeps telemetry history and inventory in bounded memory;
they disappear on hub restart. Accounts, collector enrollment, machine names/tags
and alert state remain on disk. Run
`rigscope configure --telemetry-mode persistent` for durable summary history, or
`rigscope configure --telemetry-mode memory` to switch back. The command asks
before stopping and restarting services; RAM history is not copied. Existing disk history is preserved while RAM mode is active.
See [Telemetry storage](telemetry-storage.md) for limits and delivery semantics.
