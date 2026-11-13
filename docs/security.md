# Security

This guide describes what Rigscope 1.0 protects against, which checks are
enforced in code, and what remains the operator's responsibility. It is a
practical threat-model summary, not a certification.

## Reporting a vulnerability

Report suspected vulnerabilities privately to
[mtrietvu@gmail.com](mailto:mtrietvu@gmail.com). Include the affected version, a
description, reproduction steps and potential impact. Redact passwords, grants,
credentials, private telemetry and other sensitive data. Do not open a public
issue before a fix or disclosure plan is agreed.

Rigscope is an early release. No guaranteed response time or long-term security
support window is offered. Use the latest release.

## Threat model

Rigscope assumes:

- The **hub is the network-facing component**. It may be exposed to the internet
  through TLS or a reverse proxy, and it holds accounts, sessions, history and
  alert configuration.
- **Collectors initiate outbound connections**. They need no inbound port and
  authenticate to exactly one hub with a per-collector credential.
- **Browsers are untrusted clients.** A signed-in user is limited to the
  machines in their scope; administrative routes additionally require the admin
  flag, which is re-checked against the user directory on every request. A
  wildcard machine scope is not administrative access.
- **Collector telemetry is untrusted input.** The hub parses every message
  strictly, enforces size, count and rate limits, rejects unknown fields, and
  never trusts a collector to describe another machine.
- **The operator controls the host.** Root, the kernel, systemd, the certificate
  files and the network are outside Rigscope's trust boundary.

The main threats addressed are credential theft and replay, cross-site request
forgery, cross-origin data theft, spoofed forwarded headers, enrollment-grant
reuse, malicious or compromised collectors, server-side request forgery through
webhooks, accidental data disclosure through telemetry, and denial of service
through unbounded queues or expensive requests.

Out of scope: a compromised root account or kernel, physical access, a
compromised release host or GitHub account beyond checksum verification, browser
extensions, and network flooding above the documented connection and rate
budgets. Rigscope does not encrypt its database at rest; use full-disk
encryption if that is part of your threat model.

## The security gate

### Host and Origin

In remote mode, every request is checked before any handler runs:

- A missing or duplicate `Host` is 400; a host that is not the canonical public
  host is 421.
- A present `Origin` must be byte-identical to the configured canonical origin,
  otherwise 403.
- Mutating methods and WebSocket upgrades require that exact Origin, except the
  collector and health endpoints, which enforce their own rules.
- One exception to the canonical Host: the collector endpoints
  (`/api/collector/connect` and `/api/collector/enroll`) also accept a loopback
  Host (`127.0.0.1`, `localhost` or `::1` on the bound port) when the TCP peer
  is loopback and the request carries no `Forwarded`, `X-Forwarded-*`,
  `X-Real-IP` or `Via` header. This keeps the hub's own collector connected
  after an origin is configured behind a proxy. Those endpoints authenticate
  with a grant or credential, never a browser session, and a request that came
  through an HTTP reverse proxy carries a forwarding header, so pages, sign-in
  and the API still require the canonical Host.

In loopback mode, mutations and upgrades require an exact same-origin `Origin`
whose host and port match the bound listener. The canonical origin is a static
configuration value; it is never derived from `X-Forwarded-Host` or another
request header. Absolute-form request targets and `CONNECT` are rejected with
400 so a client-chosen authority cannot become the request's Host.

### Forwarded headers

Forwarded values are evidence only when the TCP peer is in `trustedProxies`.
Exactly one header family is authoritative (`x-forwarded` or RFC 7239
`forwarded`); the other is ignored and stripped. Rigscope walks
`X-Forwarded-For` from the right, skipping trusted proxies, and uses the first
untrusted address. A malformed hop falls back to the TCP peer. RFC 7239 parsing
is strict: one syntax error discards every forwarded fact for that request.

Loopback is admitted to a plaintext trusted-proxy listener for health probes and
local operator use, but it is **not** a trusted forwarded-header source. A
plaintext non-loopback listener accepts connections only from configured proxies
and loopback. See [TLS and trusted proxies](configuration.md#tls-and-trusted-proxies).

### Sessions, cookies and CSRF

- Cookies are `HttpOnly`, `SameSite=Strict`, `Path=/`, with a fixed 12-hour
  lifetime. HTTPS sessions also use `Secure` and the `__Host-` prefix, so a
  copied loopback cookie cannot authenticate against a remote origin.
- The cookie value is an opaque random token. The database stores only its
  SHA-256 hash.
- At most 256 sessions exist; creating one beyond the cap evicts the oldest.
- Logout, expiry, account changes, user deletion and collector revocation end
  the affected session. Ending a session immediately closes its SSE streams and
  process-detail interests and releases collector detail leases.
- Authenticated mutations require an `X-CSRF-Token` header matching the
  session's token in constant time, in addition to a trusted Origin.
- Login and logout require a trusted Origin. A failed logout with a bad
  Origin/CSRF does not clear the cookie; a missing session does, so a stale
  credential cannot loop.

### Response hardening

Every hub response carries:

```text
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
X-Frame-Options: DENY
```

HTML documents also carry a Content-Security-Policy with `default-src 'self'`,
`frame-ancestors 'none'`, `base-uri 'none'`, `form-action 'self'`,
`object-src 'none'`, `connect-src 'self'`, and `script-src 'self'` plus SHA-256
hashes for the exact inline scripts in that document. `style-src` allows inline
styles because the UI sets style attributes for charts; scripts do not allow
`unsafe-inline`.

HTTPS responses send `Strict-Transport-Security: max-age=31536000`. HSTS is sent
for direct TLS and for an HTTPS canonical origin behind a trusted proxy, and is
not sent for a plaintext public origin. It does not include subdomains or
preload.

### Login cost and rate limits

Passwords are stored as argon2id PHC strings with 32 MiB of memory, 3 passes, 2
lanes, a 16-byte salt and a 32-byte key. Verification refuses stored hashes that
ask for more work than the documented bounds, so a corrupted row cannot make the
hub allocate unlimited memory or spin for minutes.

Unknown usernames perform the same password work as known ones, and failures are
reported generically as `invalid username or password`.

Login attempts are limited by:

- a hub-wide token bucket with a burst of 10 and one attempt recovered every
  500 ms;
- at most 4 concurrent password verifications;
- per-client-IP and per-username buckets, bounded to 512 entries with a
  10-minute idle expiry;
- an exponential cooldown after repeated failures, capped at one second.

A login request also has a 5-second read deadline, a 4 KiB body limit, and
bounded username and password lengths. Enrollment has its own budget: a burst of
30, one request per 200 ms, and 16 concurrent requests.

### Collector credentials and enrollment

- A collector authenticates with a long-lived credential in an `Authorization`
  header, never in a URL. The hub stores only a SHA-256 hash and uses it for
  authentication; a sealed recovery copy exists so an interrupted enrollment can
  return the same credential, not to authenticate with the plaintext value.
- Enrollment uses a short-lived, single-use grant. A retry with the same grant,
  request id and recovery secret returns the same credential for up to 24
  hours, which is how a lost enrollment response is recovered. That window,
  recorded when the grant was spent, is the authority for a spent grant; an
  unspent grant's expiry is checked before it can take part in any recovery, so
  an expired grant can never be used as a key.
- Automatic local pairing stores only a grant hash in the hub's bootstrap file
  and binds the grant to the collector's installation id. Redemption requires a
  socket the hub attributes to its own machine and that exact collector id, for
  first enrollment and recovery retries alike. "Local" means a loopback peer, or
  a peer whose address equals the listening socket's local address: a collector
  configured with the hub's origin dials a name that resolves to one of the
  host's addresses, and that self-connection is the only way an origin-based
  destination can pair. Forwarded headers never widen the rule, and they close
  it: the security layer records whether any `Forwarded`, `X-Forwarded-*`,
  `X-Real-IP` or `Via` header was present before stripping it, and a redemption
  that carried one is refused, as is one whose direct peer is a configured
  trusted proxy. A reverse proxy on the same machine is therefore refused even
  though its own connection arrives from loopback. A TCP-level proxy — a stream
  proxy or `ssh -L` — forwards a remote caller's connection to loopback with no
  header at all and cannot be detected; do not expose an enrollment listener
  that way.
  The hub imports a pending bootstrap file on the first enrollment request as
  well as at startup, so adding a collector to a running hub needs no restart.
  A reinstall keeps the saved recovery proof, so rotating an expired grant does
  not orphan a credential issued to a lost response. Cross-grant recovery is
  limited to the installer's colocated grant, bound to the same collector and
  still unexpired, and its matched grant must carry the same purpose and
  binding; an administrator-issued grant is never a recovery vehicle.
- The installer publishes the bootstrap and enrollment files through the
  installation ownership manifest. The hub reads the bootstrap through a
  no-follow directory descriptor, requires a private regular file, bounds its
  size and refuses to overwrite a file the manifest does not own.
- Every denial is the same 403 `enrollment denied`: unknown, expired, reused and
  duplicate-collector cases are not distinguished to the caller.
- The enrollment and connect endpoints reject any `Origin` header. The collector
  connects over TLS unless it is talking to literal loopback.
- A revoked collector's live WebSocket session is closed and it is removed from
  live state. Its machine record and history stay until an administrator
  removes it: **Remove** (offered only for revoked collectors) deletes the
  collector record, the machine's name and tags, its alert rules, incidents and
  pending notifications, and all of its stored history, RAM or persistent. It
  is audited and cannot be undone. A collector cannot claim another collector's
  identity; a mismatched `hello.collectorId` ends the session.

### Collector trust and payload bounds

The hub bounds message size, record count, frame rate, output queue, connection
count and per-session timeouts. Protocol messages use strict decoders: unknown
fields, out-of-range numbers, invalid timestamps and invalid enum values are
rejected. Records are deduplicated by `(collectorId, runId, sequence)`.
Unavailable readings carry an explicit reason and no value; a fabricated zero is
never accepted as a substitute.

The hub acknowledges a sample only after its storage transaction commits.
In memory mode that commit is in RAM and is lost on hub restart; in persistent
mode it is a durable disk commit. Admission to the bounded queue alone does
not trigger acknowledgment. Replay is bounded to 24 hours; older records are
rejected permanently rather than risking a pruned deduplication receipt. See [Telemetry storage](telemetry-storage.md).

### Webhook SSRF protection

Alert webhooks accept HTTPS URLs, or HTTP only for a controlled loopback sink.
Before every delivery the hub resolves the target itself and refuses loopback,
private, link-local, multicast and other non-public addresses. The check is on
the address actually dialed, so a DNS-rebinding answer cannot pass; redirects are
refused and proxy environment variables are ignored. A loopback HTTP sink may
reach loopback only. Webhook URLs, headers and HMAC secrets are private: they
are never logged or returned by the API. Delivery is at-least-once, with an
idempotency key and a signature header when a secret is configured.

### Offline recovery

`rigscope reset-password` takes the same exclusive lock as the running hub, so
it never runs alongside it. When the hub service holds the lock, the command
asks before stopping it (or accepts `--yes`), then changes the password and
revokes that user's sessions in one transaction and starts the hub again. The
new password is chosen at a no-echo prompt or read with `--password-stdin`, and
asked for before anything stops; a generated one is printed once.

The first owner account is created by `install`, not by the hub: the installer
writes only its argon2id hash to a private (0600, hub-owned) `users.json` that
the hub imports once on first start. No password reaches the journal. It never reveals
or overwrites another account's password.

## Runtime power management

On hybrid laptops the collector must not deliberately wake a suspended discrete
GPU. Every GPU hardware access goes through a per-device lease. The collector
reads `power/runtime_status` before deciding, runs `nvidia-smi` and the private
PCI helper under that lease, and releases a command's leases only after its
child has actually been reaped. A revocation drains before a quiet window
starts, and the quiet window is measured from the end of the drain.

Immediately before each access the status is re-read under the lease, and a
device the kernel no longer reports usable is refused. An `unsupported`
runtime-PM status only means runtime PM is disabled; it authorizes access only
when the cached `power_state` reads exactly `D0`. A `D3*` reading is treated as
suspended, and a missing or unrecognised reading fails closed as unknown.

**Documented residual risk:** the window between that last re-check and the
access cannot be closed from user space, because `nvidia-smi` and sysfs offer no
non-waking power reference. Losing that race can wake a suspended device once.
The reading is then reported unavailable with a power-state reason and sampling
resumes. This is accepted as a best-effort guarantee, not an absolute one.

The PCI register helper runs as a separate bounded process (`rigscope internal
pci-read`) with a hard timeout, so a wedged device cannot stall or kill the
collector. It maps the BAR read-only. Register access is opt-in and installed by
`--gpu-registers`; a normal install never grants it. In user scope the binary
and units stay owned by you and only the root-owned udev rule is applied through
`sudo`, so no user-writable file is ever executed as root. An existing rule is
adopted only when it grants the group the install needs; a rule for another
account's group is left untouched and register sampling stays off. A shared
`rigscope-gpu` rule is never rewritten to a private group, and is always
recorded as adopted even when a user install wrote it. The install probe opens
and maps `resource0` read-only without reading a register word, so it proves
permission without MMIO and does not wake a suspended GPU. Uninstall removes
only an owned rule for your private group that Rigscope wrote, still recognises,
and whose content and group still match the manifest; anything else is left in
place.

## Read-only monitoring

Rigscope is a monitor. It does not kill or signal processes, change priorities
or affinities, start or stop other services, write to sysfs, or control hardware.
Process detail is request-scoped and expires. Environment variables are never
collected; command lines are omitted unless the collector's local sharing policy
allows them. Process detail is not persisted.

## Operator responsibilities

Rigscope cannot protect a deployment from its own environment. You are
responsible for:

- keeping history, backups and the hub database on a trusted filesystem,
  preferably encrypted at rest;
- issuing and renewing TLS certificates, or correctly configuring the reverse
  proxy and `trustedProxies`;
- restricting network access to the hub and protecting the proxy-to-hub link;
- keeping the operating system, systemd and the Rigscope release current;
- reviewing grants and accounts, and revoking collectors and users that should
  no longer have access;
- treating the enrollment grant, collector credential and owner password as
  secrets.

## Related

- [Configuration](configuration.md) for TLS, origins and trusted proxies.
- [Installation](installation.md) for scopes, service accounts and recovery.
- [Monitoring and alerts](monitoring.md) for what is collected and shared.
- [Telemetry storage](telemetry-storage.md) for retention and loss semantics.
