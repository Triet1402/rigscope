# Monitor NVIDIA and AMD GPUs on Linux with Rigscope

Rigscope shows GPU inventory and the telemetry your Linux drivers expose: utilization, dedicated memory, temperature, and power where supported. The same dashboard includes CPU, memory, disk, network, and processes, so you can compare GPU activity with the rest of a machine.

Support depends on the adapter, driver, kernel interfaces, and collector permissions. Detecting a GPU does not guarantee that every counter is readable. Missing readings stay unavailable rather than becoming zeros.

## NVIDIA: check nvidia-smi

NVIDIA collection requires `nvidia-smi`. On the monitored machine, run:

```sh
nvidia-smi
```

Confirm that it can see the adapter under the user running the collector. If the command is missing or fails, resolve the driver or permission problem first. Rigscope can collect NVIDIA utilization, dedicated memory, temperature, power, and compute-process memory when available.

NVIDIA adapters use GPU UUIDs for identity. Model names come from NVIDIA's reported information. In History, GPU cards appear when stored series contain readings; an inventory-only placeholder does not create an extra historical card.

## AMD: check driver-provided counters

AMD collection uses readable amdgpu sysfs and hwmon interfaces. Discrete adapters can expose busy percentage, dedicated VRAM, temperature, and power. Integrated GPUs expose busy percentage and available hardware sensors, but available counters vary.

Rigscope reports only driver-provided VRAM and does not estimate shared system memory. On AMD APUs, a reported power counter can include CPU/SoC power; do not interpret it as GPU-only power or whole-machine power.

Use `rigscope status` and the collector's journal on an installed machine to check that collection is running. If a GPU or sensor is missing, check the [sharing policy](/docs/configuration/#sharing-policy) and the [GPU temperature notes](/docs/monitoring/#gpu-junction-and-vram-temperatures) before enabling register readings. Rigscope does not install a GPU driver or make an unsupported counter available.

## Intel and other display adapters

Intel support is partial: available hwmon temperature and power sensors and optional driver counters may appear, while most utilization readings remain unavailable. Intel sensor support does not imply utilization or per-process GPU-memory support. ASPEED and other display adapters provide inventory without a telemetry backend.

Collection supports Linux. macOS, Windows, and Apple GPU collection are not supported. For supported platforms and installation requirements, read the [installation guide](/docs/installation/).

## Compare live GPU usage with History

After [installing Rigscope](/docs/installation/), open the machine's Overview and inspect the available GPU readings. Compare activity with CPU and memory rather than assuming low GPU utilization identifies a particular bottleneck. A workload can be limited elsewhere, and a missing counter cannot establish utilization.

Use History to inspect stored GPU series across a workload. Temperature, watts, and memory use have different units and should be interpreted separately. GPU power and CPU package power are distinct measurements; neither is whole-machine wall power.

Process inspection is live and is not retained in History. NVIDIA compute-process memory can help inspect supported workloads, but per-process GPU telemetry is not universal across vendors.

## Configure alerts for available sensors

In Alerts, review hot-GPU or resource presets for the selected machine. Choose thresholds and durations appropriate to the adapter and workload; presets are starting points, not manufacturer safety limits. Check unavailable-reading rules separately so a missing sensor does not look like a healthy temperature.

For several GPU machines, follow [multi-machine setup](/guides/monitor-multiple-linux-machines/). See [monitoring and alerts](/docs/monitoring/) for retention, sharing, and notifications.
