/**
 * Presentational primitives for the resource panels. Tokens come from the
 * existing theme (metric hues, hairline borders, tabular numerals); nothing
 * here introduces a second component language.
 */

export function Meter({
  label,
  valueLabel,
  percent,
  tone,
  ariaLabel,
  note,
}: {
  label: string;
  valueLabel: string;
  percent: number | null;
  tone: "cpu" | "mem" | "swap" | "gpu" | "disk";
  ariaLabel: string;
  note?: string;
}) {
  const clamped = percent === null ? null : Math.min(100, Math.max(0, percent));
  const color =
    tone === "cpu" ? "var(--cpu)"
    : tone === "mem" ? "var(--mem)"
    : tone === "swap" ? "var(--swap)"
    : tone === "gpu" ? "var(--accent)"
    : "var(--mem)";
  return (
    <div className="min-w-0">
      <div className="flex flex-wrap items-baseline justify-between gap-x-2 gap-y-0.5">
        <span className="text-xs text-muted-foreground">{label}</span>
        <span className="tnum text-xs text-foreground">{valueLabel}</span>
      </div>
      <div
        role="img"
        aria-label={ariaLabel}
        className="mt-1.5 h-1.5 w-full overflow-hidden rounded-full bg-card-strong"
      >
        {clamped === null ? (
          <div className="h-full w-full rounded-full border border-dashed border-border" />
        ) : (
          <div
            className="h-full rounded-full"
            style={{width: `${clamped}%`, backgroundColor: color}}
          />
        )}
      </div>
      {note ? <p className="mt-1.5 text-xs text-muted-foreground">{note}</p> : null}
    </div>
  );
}

export function Fact({label, value}: {label: string; value: string}) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="tnum min-w-0 truncate text-right text-xs text-foreground">{value}</dd>
    </div>
  );
}

export function Note({children}: {children: React.ReactNode}) {
  return <p className="text-xs text-muted-foreground">{children}</p>;
}

export function UnavailableNote({reason}: {reason: string}) {
  return <p className="text-xs text-warn">Unavailable — {reason}</p>;
}

export function Subheading({children}: {children: React.ReactNode}) {
  return (
    <h3 className="text-xs font-medium text-muted-foreground">{children}</h3>
  );
}
