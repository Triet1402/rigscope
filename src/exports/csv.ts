/**
 * Browser-safe CSV serialization and download helpers for Rigscope exports.
 *
 * CSV is consumed by spreadsheets, so text cells are protected from formula
 * injection and RFC 4180 quoting is applied. Numeric cells stay numeric: a
 * value such as -1 is emitted as `-1`, never as a protected text cell. Null
 * and non-finite numbers become empty cells so a missing reading can never
 * look like a measured zero.
 */

export type CsvValue = string | number | boolean | null | undefined;

/**
 * Spreadsheet applications evaluate a cell beginning with `=`, `+`, `-` or `@`
 * (and some variants after leading whitespace or a control character). Prefix
 * such text with an apostrophe so it is displayed as text instead of executed
 * as a formula. Numbers are never passed through here.
 */
const FORMULA_PREFIX = /^[\u0009\u000a\u000d ]*[=+\-@]/;
const CONTROL_PREFIX = /^[\u0009\u000a\u000d]/;

export function spreadsheetSafeText(value: string): string {
  if (value.length === 0) return value;
  return FORMULA_PREFIX.test(value) || CONTROL_PREFIX.test(value) ? `'${value}` : value;
}

/** Quote one CSV cell when needed and protect text from formula evaluation. */
export function csvCell(value: CsvValue): string {
  if (value === null || value === undefined) return "";
  if (typeof value === "number") return Number.isFinite(value) ? String(value) : "";
  if (typeof value === "boolean") return value ? "true" : "false";

  const text = spreadsheetSafeText(value);
  return /[",\t\r\n]/.test(text) ? `"${text.replace(/"/g, '""')}"` : text;
}

export function csvRow(values: readonly CsvValue[]): string {
  return values.map(csvCell).join(",");
}

/** RFC 4180 line endings with a trailing newline. */
export function csvDocument(rows: ReadonlyArray<readonly CsvValue[]>): string {
  return `${rows.map(csvRow).join("\r\n")}\r\n`;
}

/**
 * Trigger a browser download from an in-memory string. The object URL is
 * revoked on the next task, after the browser has had a chance to begin the
 * download. No server request is made.
 */
export function downloadFile(filename: string, contents: string, mimeType: string): void {
  if (typeof document === "undefined") return;
  const blob = new Blob([contents], { type: mimeType });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = filename;
  link.rel = "noopener";
  link.style.display = "none";
  document.body.append(link);
  link.click();
  link.remove();
  globalThis.setTimeout(() => URL.revokeObjectURL(url), 0);
}

export const CSV_MIME = "text/csv;charset=utf-8";
export const JSON_MIME = "application/json;charset=utf-8";
