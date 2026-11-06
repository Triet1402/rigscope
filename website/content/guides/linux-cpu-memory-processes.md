# Understand Linux CPU, memory, and process readings

When a Linux machine feels slow, start with its overall resource readings and then inspect the processes behind them. Rigscope provides machine usage, per-core CPU readings, memory, pressure metrics where available, and live process details in a self-hosted dashboard.

If you have not installed it yet, follow the [Linux installation guide](/docs/installation/). Monitoring is read-only: Rigscope does not kill processes, change priorities, or control remote services.

## Compare machine CPU with process CPU

Machine CPU is normalized across all logical cores. Process CPU uses **100% per logical core**. A process using more than 100% can be using several cores; its percentage is not directly comparable to the machine's normalized percentage.

Start in Overview and inspect both total usage and individual cores. Modest overall CPU usage can coexist with one busy core. Open Processes to inspect current consumers and compare readings over time rather than treating a single sample as a diagnosis.

In History, select **CPU cores** to compare cores across a recorded interval. Larger selections use a heatmap. A core's chart and an overall average answer different questions: whether particular cores are busy and how much of the machine's total CPU capacity is in use.

## Read memory alongside swap and pressure

Check memory usage, swap, and available pressure readings together. Memory use on its own does not establish why an application is slow. Compare the time of the slowdown with changes in resource readings, then inspect the processes that are active now.

Linux pressure readings describe time spent stalled on a resource. Rigscope reports PSI `some avg10` for CPU, and `some avg10` plus `full avg10` for memory and I/O when available. These are percentages over a recent ten-second window, not percentages of bytes used. For memory and I/O, `full` measures simultaneous stalls of all non-idle tasks.

Use the [sensor reference](/docs/bottlenecks/) for units and sources. Unreadable files, unavailable kernel interfaces, or invalid counter intervals produce unavailable readings rather than zeros. An empty sensor value is not evidence of an idle resource.

## Use History to establish when the change began

Select the machine and time range in History. Hover over a chart to inspect a recorded bucket; its time marker follows across charts in the same view. With a chart focused, use Left/Right to move between readings, Home/End for the first or last reading, and Escape to dismiss the marker.

By default, raw samples are retained for 48 hours, one-minute summaries for 30 days, and one-hour summaries for 400 days. Summaries retain minimum and maximum values. Older data is less granular, and bounded queries may return partial results, which the UI marks.

Process details are fetched while viewers request them and are never persisted. You can use History to identify when machine usage changed, but it cannot reconstruct a process list from yesterday. Export current process data or loaded historical readings when you need a separate record; see [exports](/docs/exports/).

## Set alerts after observing normal behavior

Use Alerts presets as starting points for CPU, memory, pressure, and other supported resources. Review both the threshold and duration against the workload. A brief compile spike may be normal while sustained pressure deserves investigation. Presets are not hardware safety limits.

Keep missing-receipt and unavailable-reading rules distinct from high-usage rules. Read [monitoring and alerts](/docs/monitoring/) for stale readings, maintenance, recovery, and webhook delivery behavior.
