/**
 * CLI presentation helpers.
 *
 * These are pure string formatters plus explicit write helpers. Nothing here
 * patches `console`, changes global streams, or decides where command output
 * belongs: callers pass a stream and an options object, so piped output, JSON
 * mode and TTY behavior stay under the command's control.
 *
 * Color is opt-in only for terminals. `NO_COLOR` (per https://no-color.org),
 * `TERM=dumb`, JSON mode and non-TTY streams all produce plain text.
 */

export type OutputStream = {
  isTTY?: boolean;
  write(chunk: string): unknown;
};

export type OutputEnvironment = Record<string, string | undefined>;

export type OutputOptions = {
  /** Force color on or off. NO_COLOR, json and TERM=dumb still win. */
  color?: boolean;
  env?: OutputEnvironment;
  /** Used for auto-detection, and as the destination of the print helpers. */
  stream?: OutputStream;
  /** Machine-readable output never contains ANSI escapes. */
  json?: boolean;
};

const RESET = '\u001b[0m';
const COLORS = {
  red: 31,
  green: 32,
  yellow: 33,
  blue: 34,
  magenta: 35,
  cyan: 36,
  gray: 90,
} as const;
const FONTS = {bold: 1, dim: 2} as const;

export type ColorName = keyof typeof COLORS;
export type FontName = keyof typeof FONTS;

/** True when terminal color is appropriate for this stream/environment. */
export function useColor(options: OutputOptions = {}): boolean {
  if (options.json) return false;
  const env = options.env ?? process.env;
  if (env.NO_COLOR !== undefined) return false;
  if (env.TERM === 'dumb') return false;
  if (options.color !== undefined) return options.color;
  return (options.stream ?? process.stdout).isTTY === true;
}

/** Aliases kept explicit for callers that name the concept differently. */
export const colorEnabled = useColor;
export const supportsColor = useColor;

export function colorize(text: string, color: ColorName, options: OutputOptions = {}): string {
  if (!useColor(options)) return text;
  return `\u001b[${COLORS[color]}m${text}${RESET}`;
}

function font(text: string, style: FontName, options: OutputOptions = {}): string {
  if (!useColor(options)) return text;
  return `\u001b[${FONTS[style]}m${text}${RESET}`;
}

export function bold(text: string, options: OutputOptions = {}): string {
  return font(text, 'bold', options);
}

export function dim(text: string, options: OutputOptions = {}): string {
  return font(text, 'dim', options);
}

/** A completed action. The check mark is present in plain output too. */
export function success(message: string, options: OutputOptions = {}): string {
  return colorize(`✔ ${message}`, 'green', options);
}

/** A recoverable problem or a detail that deserves attention. */
export function warning(message: string, options: OutputOptions = {}): string {
  return colorize(`! Warning: ${message}`, 'yellow', options);
}

/** A failed command. The lead-in stays stable in plain and colored output. */
export function error(message: string, options: OutputOptions = {}): string {
  return colorize(`✖ Error: ${message}`, 'red', options);
}

/** Informational text is intentionally uncolored. */
export function info(message: string, _options: OutputOptions = {}): string {
  return message;
}

/** Help text is tinted on terminals and plain when piped. */
export function help(message: string, options: OutputOptions = {}): string {
  return colorize(message, 'cyan', options);
}

/** A stable `Label: value` status line with an emphasized label. */
export function status(label: string, value: string, options: OutputOptions = {}): string {
  return `${bold(label, options)}: ${value}`;
}

export function formatError(cause: unknown, options: OutputOptions = {}): string {
  const message = cause instanceof Error ? cause.message : String(cause);
  return error(message, options);
}

/** Write exactly one line, preserving an existing trailing newline. */
export function writeLine(stream: OutputStream, message: string): void {
  stream.write(message.endsWith('\n') ? message : `${message}\n`);
}

export function printSuccess(message: string, options: OutputOptions = {}): void {
  writeLine(options.stream ?? process.stdout, success(message, options));
}

export function printWarning(message: string, options: OutputOptions = {}): void {
  writeLine(options.stream ?? process.stderr, warning(message, options));
}

export function printError(message: string, options: OutputOptions = {}): void {
  writeLine(options.stream ?? process.stderr, error(message, options));
}

export function printInfo(message: string, options: OutputOptions = {}): void {
  writeLine(options.stream ?? process.stdout, info(message, options));
}

export function printStatus(label: string, value: string, options: OutputOptions = {}): void {
  writeLine(options.stream ?? process.stdout, status(label, value, options));
}

export function printHelp(message: string, options: OutputOptions = {}): void {
  writeLine(options.stream ?? process.stdout, help(message, options));
}
