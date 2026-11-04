# System installation references

These files illustrate dedicated `rigscope-hub` and `rigscope-collector` accounts, systemd units and read-only package-energy access. The renderer in `src/cli/system.ts` is authoritative; do not install examples blindly.

See [system installation](../../docs/installation.md#system-installation) for planning, `--apply`, independent component controls and RAPL permission limits. The collector needs GPU device access and does not use `PrivateDevices=yes`; the hub does. Both run without root privileges, with private state and bounded writable paths.
