/**
 * Deterministic, filesystem-safe export filenames. Machine labels come from
 * user-controlled or collector-controlled text, so they are reduced to a
 * short slug rather than trusted as part of a filename.
 */

export type ExportKind = "history" | "processes";

export function filenameSlug(value: string, fallback = "machine"): string {
  const slug = value
    .normalize("NFKD")
    .replace(/[\u0300-\u036f]/g, "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 48)
    .replace(/-+$/g, "");
  return slug.length > 0 ? slug : fallback;
}

/** UTC timestamp used in filenames: 20261103T101530Z. */
export function compactTimestamp(ms: number): string {
  if (!Number.isFinite(ms)) return "unknown-time";
  const iso = new Date(ms).toISOString();
  return iso.replace(/[-:]/g, "").replace(/\.\d{3}Z$/, "Z");
}

export function exportFilename(
  kind: ExportKind,
  machineLabel: string,
  atMs: number,
  extension: "csv" | "json",
): string {
  return `rigscope-${kind}-${filenameSlug(machineLabel)}-${compactTimestamp(atMs)}.${extension}`;
}
