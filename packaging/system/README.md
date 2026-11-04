# System installation references

These files illustrate dedicated `rigscope-hub` and `rigscope-collector` accounts, systemd units and read-only package-energy access. The renderer in `src/cli/system.ts` is authoritative; do not install examples blindly.

See [System installation](../../docs/system-installation.md) for planning, `--apply`, component controls and CPU package-power permissions. The collector needs GPU device access and does not use `PrivateDevices=yes`; the hub does. Both run without root privileges.
