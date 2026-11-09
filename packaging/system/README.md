# System installation references

These files illustrate dedicated `rigscope-hub` and `rigscope-collector` accounts, systemd units, read-only package-energy access and read-only NVIDIA GPU BAR0 access. The renderer in `src/cli/system.ts` is authoritative; do not install examples blindly. The two `90-*.rules` files are installed by `sudo rigscope cpu-power enable` and `sudo rigscope gpu-temps enable`.

See [System installation](../../docs/system-installation.md) for planning, `--apply`, component controls and CPU package-power permissions. The collector needs GPU device access and does not use `PrivateDevices=yes`; the hub does. Both run without root privileges.
