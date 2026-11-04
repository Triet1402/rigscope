# Export monitoring data

History and Processes provide CSV and JSON download controls. Exports use the data
already loaded in the browser; downloading does not request more history or trigger
a process scan. Access remains scoped to the signed-in user's machines.

## History

Exports include the selected metrics and visible series. Hidden GPU cards and
legend series are excluded. The document records the machine, time range, units,
load time, resolution and partial-result status. Rolled-up buckets retain their
minimum, maximum and average; these are not individual raw samples.

Unavailable numeric values remain empty in CSV and null in JSON. Recorded gaps
include their interval and available reason/drop count. Selected metrics without
stored data are identified separately. The JSON summary's
`averageOfBucketAverages` is an average of the loaded buckets, not a weighted
average of all underlying raw samples.

The CSV begins with metadata rows, followed by a `record_type` table containing
`bucket` and `gap` records. JSON includes equivalent structured metadata and series.

## Processes

Exports follow the current search, sort and view. Application view includes each
matching group's processes; tree view retains depth. Snapshot capture time,
receipt time, freshness, unreadable-process count and collector truncation are
included. A paused or stale snapshot is labeled accordingly.

The exported fields are process name, PID, user, application/group, tree depth,
CPU percentage, resident memory, GPU memory, read/write rates and thread count.
Command arguments and environment data are not exported. CSV begins with snapshot
metadata, followed by the process table; JSON preserves the same scope explicitly.

## Files

Downloads use a machine slug and UTC timestamp. Text cells that could be treated
as spreadsheet formulas receive an apostrophe prefix in CSV. Numeric measurements
remain numeric. JSON preserves original text without spreadsheet-specific changes.
