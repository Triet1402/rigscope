# Security policy

Report suspected vulnerabilities privately to
[mtrietvu@gmail.com](mailto:mtrietvu@gmail.com).

Include the affected Rigscope version, a description of the issue, reproduction
steps and its potential impact. Redact passwords, enrollment grants, collector
credentials, private telemetry and other sensitive information. Keep
vulnerability details out of public issues until a fix or disclosure plan has
been agreed.

Rigscope is an early release. No guaranteed response time or long-term security
support window is offered. Use the latest available release.

## What is in scope

Rigscope 1.0 is a single static Go binary. The hub is the network-facing
component; collectors connect outbound and authenticate with a per-collector
credential. In scope are the hub's HTTP, SSE and WebSocket endpoints, browser
authentication and authorization, enrollment, alert webhooks, the collector's
sampling and delivery, the installer/upgrade/uninstall commands, and the
documented security guarantees.

Rigscope is a read-only monitor: it does not control processes or hardware.
The collector's opt-in GPU register access is read-only and isolated in a
bounded helper process.

## What is not in scope

- A compromised root account, kernel, systemd or container runtime.
- Physical access to a machine.
- A compromised release host or GitHub account beyond the SHA-256 verification
  Rigscope performs.
- Browser extensions, or a browser that ignores cookie and same-origin rules.
- Network flooding beyond the documented connection, request and message
  budgets.
- Data remanence on storage you do not encrypt. Rigscope does not encrypt its
  SQLite database at rest.

## Read more

The threat model, the enforced security gate, runtime power-management
guarantees and the documented residual race are described in
[docs/security.md](docs/security.md).
