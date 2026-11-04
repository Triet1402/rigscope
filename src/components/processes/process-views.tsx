"use client";

/**
 * Dense application and process tables for the Processes screen.
 *
 * Columns use a fixed table layout with explicit widths, so live updates
 * never reflow them. Each header has a drag handle (double-click resets,
 * arrow keys nudge) and proportions persist in the browser per table kind.
 * Column proportions follow the available width when the window is resized.
 */

import { Fragment, useEffect, useRef, useState } from "react";
import { ArrowDown, ArrowUp, ChevronRight, ChevronsUpDown } from "lucide-react";

import type { ProcessRow } from "@/contracts";
import type { AppGroup, SortKey, TreeNode } from "@/processes/aggregate";
import {
  formatInteger,
  groupKindLabel,
  metricDisplay,
  rowKey,
  sortAriaValue,
  type MetricKind,
  type SortState,
} from "@/processes/presentation";
import {
  APPLICATION_COLUMNS,
  MAX_COLUMN_WIDTH,
  MIN_COLUMN_WIDTH,
  PROCESS_COLUMNS,
  clampWidth,
  useColumnWidths,
  type ColumnDef,
  type ColumnWidths,
  type TableKind,
} from "./process-prefs";

type SortProps = {
  sort: SortState;
  onSort: (key: SortKey) => void;
};

const CELL = "overflow-hidden text-ellipsis whitespace-nowrap border-b border-border px-2 py-2 align-middle";
const NUMBER_CELL = `${CELL} tnum text-right text-xs`;
const HEADER =
  "relative overflow-hidden whitespace-nowrap border-b border-border px-2 py-2 text-left text-xs font-normal text-muted-foreground";

function widthOf(column: ColumnDef, widths: ColumnWidths): number {
  return widths[column.id] ?? column.width;
}

function ResizeHandle({
  label,
  width,
  onChange,
  onReset,
}: {
  label: string;
  width: number;
  onChange: (width: number) => void;
  onReset: () => void;
}) {
  const drag = useRef<{ x: number; width: number } | null>(null);
  return (
    <div
      role="separator"
      aria-orientation="vertical"
      aria-label={`Resize ${label} column`}
      aria-valuenow={width}
      aria-valuemin={MIN_COLUMN_WIDTH}
      aria-valuemax={MAX_COLUMN_WIDTH}
      tabIndex={0}
      title="Drag to resize · double-click to reset"
      onPointerDown={(event) => {
        if (event.button !== 0) return;
        event.preventDefault();
        event.stopPropagation();
        event.currentTarget.setPointerCapture(event.pointerId);
        drag.current = { x: event.clientX, width };
      }}
      onPointerMove={(event) => {
        if (drag.current === null) return;
        onChange(clampWidth(drag.current.width + event.clientX - drag.current.x));
      }}
      onPointerUp={(event) => {
        if (drag.current !== null) {
          onChange(clampWidth(drag.current.width + event.clientX - drag.current.x));
        }
        drag.current = null;
        if (event.currentTarget.hasPointerCapture(event.pointerId)) {
          event.currentTarget.releasePointerCapture(event.pointerId);
        }
      }}
      onPointerCancel={() => {
        drag.current = null;
      }}
      onLostPointerCapture={() => {
        drag.current = null;
      }}
      onClick={(event) => event.stopPropagation()}
      onDoubleClick={(event) => {
        event.stopPropagation();
        onReset();
      }}
      onKeyDown={(event) => {
        if (event.key === "ArrowLeft") {
          event.preventDefault();
          onChange(clampWidth(width - 16));
        } else if (event.key === "ArrowRight") {
          event.preventDefault();
          onChange(clampWidth(width + 16));
        }
      }}
      className="group/handle absolute inset-y-0 right-0 z-10 flex w-2.5 cursor-col-resize touch-none select-none justify-end"
    >
      <span
        aria-hidden
        className="my-2 w-px bg-border transition-colors group-hover/handle:bg-accent group-focus-visible/handle:bg-accent"
      />
    </div>
  );
}

function HeaderCell({
  column,
  sort,
  onSort,
  width,
  onResize,
  onReset,
}: SortProps & {
  column: ColumnDef;
  width: number;
  onResize: (width: number) => void;
  onReset: () => void;
}) {
  const right = column.align === "right";
  const sortKey = column.sortKey;
  const direction = sortKey !== undefined ? sortAriaValue(sort, sortKey) : "none";
  const active = direction !== "none";
  const Icon = !active ? ChevronsUpDown : sort.dir === "asc" ? ArrowUp : ArrowDown;
  return (
    <th
      scope="col"
      aria-sort={sortKey !== undefined ? direction : undefined}
      className={right ? `${HEADER} text-right` : HEADER}
    >
      {sortKey !== undefined ? (
        <button
          type="button"
          onClick={() => onSort(sortKey)}
          className={[
            "inline-flex min-h-6 max-w-full items-center gap-1 rounded-sm transition-colors hover:text-foreground",
            right ? "w-full justify-end pr-1" : "",
            active ? "text-foreground" : "",
          ]
            .filter(Boolean)
            .join(" ")}
        >
          <span className="truncate">{column.label}</span>
          <Icon className={active ? "h-3 w-3 shrink-0" : "h-3 w-3 shrink-0 opacity-40"} aria-hidden />
        </button>
      ) : (
        <span className={right ? "block truncate pr-1" : "block truncate"}>{column.label}</span>
      )}
      <ResizeHandle label={column.label} width={width} onChange={onResize} onReset={onReset} />
    </th>
  );
}

function ResizableTable({
  table,
  columns,
  label,
  caption,
  sort,
  onSort,
  children,
}: SortProps & {
  table: TableKind;
  columns: readonly ColumnDef[];
  label: string;
  caption: string;
  children: React.ReactNode;
}) {
  const [widths, setWidths] = useColumnWidths(table);
  const container = useRef<HTMLDivElement>(null);
  const [availableWidth, setAvailableWidth] = useState(0);
  useEffect(() => {
    const element = container.current;
    if (!element) return;
    const observer = new ResizeObserver(([entry]) => setAvailableWidth(entry.contentRect.width));
    observer.observe(element);
    return () => observer.disconnect();
  }, []);
  const minimum = (column: ColumnDef) => column.id === "name" ? 160 : MIN_COLUMN_WIDTH;
  const totalMinimum = columns.reduce((sum, column) => sum + minimum(column), 0);
  const total = columns.reduce((sum, column) => sum + widthOf(column, widths), 0);
  const target = Math.max(availableWidth || total, totalMinimum);
  const rendered: Record<string, number> = {};
  let remaining = target;
  let flexible = [...columns];
  while (flexible.length > 0) {
    const weight = flexible.reduce((sum, column) => sum + widthOf(column, widths), 0);
    const constrained = flexible.filter((column) => remaining * widthOf(column, widths) / weight < minimum(column));
    if (constrained.length === 0) {
      for (const column of flexible) rendered[column.id] = remaining * widthOf(column, widths) / weight;
      break;
    }
    for (const column of constrained) {
      rendered[column.id] = minimum(column);
      remaining -= minimum(column);
    }
    flexible = flexible.filter((column) => !constrained.includes(column));
  }
  const resize = (column: ColumnDef, width: number) => {
    const index = columns.indexOf(column);
    const neighbor = columns[index + 1] ?? columns[index - 1];
    const delta = Math.max(minimum(column) - rendered[column.id],
      Math.min(width - rendered[column.id], rendered[neighbor.id] - minimum(neighbor)));
    setWidths({ ...rendered, [column.id]: rendered[column.id] + delta, [neighbor.id]: rendered[neighbor.id] - delta });
  };
  return (
    <div ref={container} className="relative min-h-0 w-full min-w-0 flex-1 overflow-auto overscroll-contain" tabIndex={0} role="region" aria-label={label}>
      <table
        className="table-fixed border-separate border-spacing-0 text-left"
        style={{ width: "100%", minWidth: totalMinimum }}
      >
        <caption className="sr-only">{caption}</caption>
        <colgroup>
          {columns.map((column) => (
            <col key={column.id} style={{ width: rendered[column.id] }} />
          ))}
        </colgroup>
        <thead className="sticky top-0 z-20 bg-card">
          <tr>
            {columns.map((column) => (
              <HeaderCell
                key={column.id}
                column={column}
                sort={sort}
                onSort={onSort}
                width={rendered[column.id]}
                onResize={(width) => resize(column, width)}
                onReset={() =>
                  setWidths((current) => {
                    const { [column.id]: _removed, ...rest } = current;
                    return rest;
                  })
                }
              />
            ))}
          </tr>
        </thead>
        <tbody>{children}</tbody>
      </table>
    </div>
  );
}

function MetricCell({
  value,
  kind,
  partial = false,
}: {
  value: number | null;
  kind: MetricKind;
  partial?: boolean;
}) {
  const display = metricDisplay(value, kind, partial);
  return (
    <td
      className={display.text === "—" ? `${NUMBER_CELL} text-muted-foreground` : NUMBER_CELL}
      title={display.title ?? undefined}
    >
      {display.atLeast ? (
        <>
          <span aria-hidden>≥ </span>
          <span className="sr-only">at least </span>
        </>
      ) : null}
      {display.text}
    </td>
  );
}

function processTitle(row: ProcessRow): string {
  const name = row.name.length > 0 ? row.name : "Unnamed process";
  return row.unit !== null ? `${name} · ${row.unit}` : name;
}

function ProcessName({ row, depth = 0 }: { row: ProcessRow; depth?: number }) {
  return (
    <th scope="row" className={`${CELL} text-left text-sm font-medium`} title={processTitle(row)}>
      <span style={depth > 0 ? { paddingLeft: `${depth * 14}px` } : undefined}>
        {depth > 0 ? <span className="sr-only">{`Nested level ${depth + 1}. `}</span> : null}
        {row.name.length > 0 ? row.name : <span className="text-muted-foreground">Unnamed process</span>}
      </span>
    </th>
  );
}

function ProcessTable({
  entries,
  sort,
  onSort,
  mode,
}: SortProps & {
  entries: Array<{ row: ProcessRow; depth: number }>;
  mode: "processes" | "tree";
}) {
  return (
    <ResizableTable
      table="processes"
      columns={PROCESS_COLUMNS}
      label={mode === "tree" ? "Process tree" : "Process list"}
      caption={
        mode === "tree"
          ? "Process tree with nesting depth, PID, user, CPU per core, resident memory, GPU memory, read and write rates, and threads"
          : "Processes with PID, user, CPU per core, resident memory, GPU memory, read and write rates, and threads"
      }
      sort={sort}
      onSort={onSort}
    >
      {entries.map(({ row, depth }) => (
        <tr key={rowKey(row)} className="transition-colors hover:bg-card-strong/60">
          <ProcessName row={row} depth={depth} />
          <td className={NUMBER_CELL}>{row.pid}</td>
          <td className={`${CELL} text-xs text-muted-foreground`} title={row.user ?? undefined}>
            {row.user ?? "—"}
          </td>
          <MetricCell value={row.cpuPercent} kind="percent" />
          <MetricCell value={row.memoryRssBytes} kind="bytes" />
          <MetricCell value={row.gpuMemoryBytes} kind="bytes" />
          <MetricCell value={row.ioReadBytesPerSec} kind="rate" />
          <MetricCell value={row.ioWriteBytesPerSec} kind="rate" />
          <td className={NUMBER_CELL}>{formatInteger(row.threads)}</td>
        </tr>
      ))}
    </ResizableTable>
  );
}

function MemberRows({ group }: { group: AppGroup }) {
  return (
    <>
      {group.members.map((member) => (
        <tr key={rowKey(member)} className="bg-card-strong/40 transition-colors hover:bg-card-strong/70">
          <th
            scope="row"
            className={`${CELL} text-left text-xs font-normal`}
            title={`${processTitle(member)} · PID ${member.pid}`}
          >
            <span className="pl-5">
              {member.name.length > 0 ? member.name : "Unnamed process"}
              <span className="tnum ml-2 text-muted-foreground">{member.pid}</span>
            </span>
          </th>
          <td className={NUMBER_CELL} />
          <MetricCell value={member.cpuPercent} kind="percent" />
          <MetricCell value={member.memoryRssBytes} kind="bytes" />
          <MetricCell value={member.gpuMemoryBytes} kind="bytes" />
          <MetricCell value={member.ioReadBytesPerSec} kind="rate" />
          <MetricCell value={member.ioWriteBytesPerSec} kind="rate" />
          <td className={`${CELL} text-xs text-muted-foreground`} title={member.user ?? undefined}>
            {member.user ?? "—"}
          </td>
        </tr>
      ))}
    </>
  );
}

function ApplicationTable({
  groups,
  sort,
  onSort,
  expanded,
  onToggleGroup,
}: SortProps & {
  groups: AppGroup[];
  expanded: ReadonlySet<string>;
  onToggleGroup: (key: string) => void;
}) {
  return (
    <ResizableTable
      table="applications"
      columns={APPLICATION_COLUMNS}
      label="Applications"
      caption="Applications grouped by systemd unit, process tree, or kernel, with process count, aggregate CPU per core, resident memory, GPU memory, read and write rates, and users"
      sort={sort}
      onSort={onSort}
    >
      {groups.map((group) => {
        const open = expanded.has(group.key);
        const label = group.label.length > 0 ? group.label : "Unnamed application";
        const users = group.users.length > 0 ? group.users.join(", ") : "—";
        return (
          <Fragment key={group.key}>
            <tr className="transition-colors hover:bg-card-strong/60">
              <th
                scope="row"
                className={`${CELL} text-left font-normal`}
                title={`${label} · ${groupKindLabel(group.kind)}`}
              >
                <button
                  type="button"
                  aria-expanded={open}
                  onClick={() => onToggleGroup(group.key)}
                  className="inline-flex min-h-6 max-w-full items-center gap-1.5 rounded-sm text-left"
                >
                  <ChevronRight
                    className={`h-3.5 w-3.5 shrink-0 text-muted-foreground transition-transform ${open ? "rotate-90" : ""}`}
                    aria-hidden
                  />
                  <span className="truncate text-sm font-medium">{label}</span>
                </button>
              </th>
              <td className={NUMBER_CELL}>{formatInteger(group.count)}</td>
              <MetricCell value={group.cpuPercent} kind="percent" partial={group.partial.cpuPercent} />
              <MetricCell value={group.memoryRssBytes} kind="bytes" partial={group.partial.memoryRssBytes} />
              <MetricCell value={group.gpuMemoryBytes} kind="bytes" partial={group.partial.gpuMemoryBytes} />
              <MetricCell
                value={group.ioReadBytesPerSec}
                kind="rate"
                partial={group.partial.ioReadBytesPerSec}
              />
              <MetricCell
                value={group.ioWriteBytesPerSec}
                kind="rate"
                partial={group.partial.ioWriteBytesPerSec}
              />
              <td className={`${CELL} text-xs text-muted-foreground`} title={users}>
                {users}
              </td>
            </tr>
            {open ? <MemberRows group={group} /> : null}
          </Fragment>
        );
      })}
    </ResizableTable>
  );
}

export function ProcessViews({
  mode,
  groups,
  rows,
  tree,
  sort,
  onSort,
  expanded,
  onToggleGroup,
}: SortProps & {
  mode: "applications" | "processes" | "tree";
  groups: AppGroup[];
  rows: ProcessRow[];
  tree: TreeNode[];
  expanded: ReadonlySet<string>;
  onToggleGroup: (key: string) => void;
}) {
  if (mode === "applications") {
    return (
      <ApplicationTable
        groups={groups}
        sort={sort}
        onSort={onSort}
        expanded={expanded}
        onToggleGroup={onToggleGroup}
      />
    );
  }
  const entries =
    mode === "tree"
      ? tree.map((node) => ({ row: node.row, depth: node.depth }))
      : rows.map((row) => ({ row, depth: 0 }));
  return <ProcessTable entries={entries} mode={mode} sort={sort} onSort={onSort} />;
}
