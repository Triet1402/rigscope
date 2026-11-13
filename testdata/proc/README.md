# procfs/sysfs fixtures

Trees here are read through `procfs.NewFSSource(os.DirFS(...))`, so the
directory root behaves like `/` for the parsers.

- `linux-host/` — one real capture from a 48-logical-core AMD EPYC host
  running Ubuntu 26.10 with a k10temp sensor, RAPL package counter and three
  real filesystems (`ext4 /`, `vfat /boot/efi`, `btrfs /srv/storage`). Only
  files the parsers read are copied. Sanitized before commit: MAC addresses,
  the MAC-derived interface name, and the kernel boot UUID were replaced with
  obviously fake values. No IP addresses, host names, serial numbers, WWNs or
  usernames appear in the copied files.
- `minimal/` — hand-written single-core host: one CPU, no swap fields, no PSI
  files, no hwmon or powercap sensors, one disk, one physical and one virtual
  interface.
- `many/` — 70 network interfaces, used to check the inventory cap of 64.

Rate-derivation edge cases (counter resets, wraparound, CPU hotplug, missing
clocks) need two consecutive snapshots, which a static tree cannot express;
those tests build mutable `fstest.MapFS` sources instead.
