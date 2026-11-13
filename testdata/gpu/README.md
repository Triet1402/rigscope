# GPU fixtures

All fixtures here are synthetic. Nothing was captured from a real machine, so
there are no GPU UUIDs, serial numbers or hostnames to sanitize.

- `nvidia/*.csv` — canned `nvidia-smi --format=csv,noheader,nounits` output,
  named after the query they answer (`gpu-*.csv`, `compute-apps.csv`). They
  cover the `[N/A]`, `[Not Supported]`, `[Unknown]` and empty-cell spellings a
  driver emits, duplicate UUID rows, short rows and CSV junk.
- `sysfs/amd-580/` — AMD PCI display device with DRM counters and one hwmon
  directory (`gpu_busy_percent`, VRAM, `temp1_input`, `power1_average`).
- `sysfs/amd-integrated/` — the same shape but with `mem_info_vram_total = 0`,
  where VRAM must stay absent rather than become shared system RAM.
- `sysfs/nvidia-mixed/` — one NVIDIA display function, one NVIDIA HDMI audio
  function and one AMD GPU, to test display-function discovery.
- `sysfs/runtime-pm/laptop/` — a hybrid-laptop discrete GPU with runtime PM
  enabled (`power/control = auto`), a non-boot display and fine-grained
  Runtime D3, plus the NVIDIA `proc` description.

Paths inside the trees are absolute sysfs paths (for example
`/sys/bus/pci/devices/0000:03:00.0/vendor`). Tests load the tree through an
`fs.FS` rooted at the fixture directory, or copy it into a mutable in-memory
filesystem so they can change a runtime status.
