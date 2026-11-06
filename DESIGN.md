---
name: Rigscope
description: A calm remote monitoring console with clear hierarchy, useful density, and coherent dark/light themes.
colors:
  background: "#181a1e"
  card: "#22252a"
  card-strong: "#2d3036"
  border: "#3a3e45"
  foreground: "#f1f2f4"
  muted-foreground: "#afb4bd"
  accent: "#3584e4"
  accent-foreground: "#ffffff"
  selected: "#1e3a5c"
  ok: "#58cc8b"
  ok-soft: "rgba(88, 204, 139, 0.15)"
  warn: "#e5b45e"
  warn-soft: "rgba(229, 180, 94, 0.15)"
  crit: "#e05e5e"
  crit-soft: "rgba(224, 94, 94, 0.15)"
  cpu: "#4c9bf5"
  mem: "#58cc8b"
  swap: "#e5b45e"
  scrollbar: "#3a3e45"
typography:
  page-title:
    fontFamily: "system-ui, ui-sans-serif, 'Noto Sans', 'Inter', sans-serif"
    fontSize: "1.125rem"
    fontWeight: 600
    letterSpacing: "-0.025em"
  panel-title:
    fontFamily: "system-ui, ui-sans-serif, 'Noto Sans', 'Inter', sans-serif"
    fontSize: "0.875rem"
    fontWeight: 500
    letterSpacing: "-0.025em"
  nav:
    fontFamily: "system-ui, ui-sans-serif, 'Noto Sans', 'Inter', sans-serif"
    fontSize: "0.875rem"
    fontWeight: 400
    letterSpacing: "-0.025em"
  support:
    fontFamily: "system-ui, ui-sans-serif, 'Noto Sans', 'Inter', sans-serif"
    fontSize: "0.75rem"
    fontWeight: 400
    lineHeight: 1.5
  data:
    fontFamily: "system-ui, ui-sans-serif, 'Noto Sans', 'Inter', sans-serif"
    fontSize: "1.25rem"
    fontWeight: 600
    fontFeature: "tabular-nums"
    letterSpacing: "-0.025em"
rounded:
  panel: "12px"
  row: "8px"
  control: "6px"
  pill: "9999px"
spacing:
  panel: "20px"
  section: "20px"
  page: "24px"
  rail: "56px"
components:
  panel:
    backgroundColor: "{colors.card}"
    textColor: "{colors.foreground}"
    rounded: "{rounded.panel}"
    padding: "20px"
  panel-title:
    textColor: "{colors.foreground}"
    typography: "{typography.panel-title}"
  empty-state:
    backgroundColor: "transparent"
    textColor: "{colors.muted-foreground}"
    rounded: "{rounded.row}"
    padding: "40px 24px"
  nav-link-selected:
    backgroundColor: "{colors.selected}"
    textColor: "{colors.foreground}"
    rounded: "{rounded.row}"
    padding: "8px 10px"
  nav-link:
    backgroundColor: "transparent"
    textColor: "{colors.muted-foreground}"
    rounded: "{rounded.row}"
    padding: "8px 10px"
  theme-toggle:
    backgroundColor: "transparent"
    textColor: "{colors.muted-foreground}"
    rounded: "{rounded.control}"
    size: "36px"
---

# Design System: Rigscope

## Overview

**Creative North Star: "The Rig-as-Console"**

A calm, modern monitoring console inspired by KDE's system monitor: clear resource
readouts, useful density, restrained surfaces, and charts that make changes easy to see.
The same visual language should support a single GPU rig and a fleet of servers.

This document provides direction and starting values. The frontmatter records the current
shared tokens; it does not freeze the palette, type scale, layout, or component inventory.
Existing CSS is the source for shipped values. Extend shared tokens coherently when a
new view needs them and update this record when those choices become reusable.

Prioritize accurate data, legibility, accessibility, and understandable interaction.
Specific styling choices below can evolve through normal implementation judgment; a
useful table, larger heading, additional surface, or new component needs no special approval.

**Key Characteristics:**
- Clear hierarchy and scan-friendly data, with room for both dense and spacious views.
- Quiet neutral surfaces, purposeful color, and recognizable metric/state meanings.
- Dark and light themes treated as full peers.
- Consistent components where tasks repeat; specialized presentation where tasks differ.
- Honest loading, empty, stale, unavailable, error, and disconnected states.

## Colors

Begin with the current neutral palette and blue accent. Reuse semantic tokens for common
roles so both themes stay coherent. The palette can expand to support new interactions,
alerts, chart series, hierarchy, and accessible contrast.

- **Primary:** accent for important actions, selection, focus, and useful emphasis. Filled
  buttons, selected surfaces, and small brand treatments are allowed when they clarify hierarchy.
- **Neutral:** background/card/card-strong and foreground/muted-foreground are the starting
  vocabulary. Add stronger dividers, intermediate text, or additional surfaces when needed;
  there is no limit of two text colors or one border strength.
- **State:** healthy, warning, critical, informational, unknown and offline states should be
  recognizable through labels/icons as well as color. Distinguish unavailable from healthy.
- **Metrics:** CPU blue, memory green, and swap amber are current defaults. Keep mappings
  predictable within and across related views; add distinguishable GPU/resource series and
  legends. A deliberate accessible palette update can revise defaults consistently.

Choose colors against their actual background in both themes. A token name alone does
not guarantee contrast. New reusable roles belong in the theme rather than scattered
component literals; exploratory values can be promoted once the design settles.

## Typography

System sans is the starting family for familiar, quick-loading UI. The existing page
heading (18px), panel heading/body (14px), supporting text (12px), and gauge value (20px)
are examples from the scaffold, not a required scale or a maximum size.

Use whatever hierarchy makes the task clear. Fleet summaries may need larger readouts;
dense tables may need compact labels; setup and error explanations need comfortable prose.
Use meaningful heading order, adequate line height, and wrapping that survives narrow
viewports and zoom. Do not shrink essential information to preserve a layout.

Use tabular numerals for aligned/changing measurements. Monospace is appropriate for
commands, addresses, identifiers, or logs when it improves reading. Include units and
consistent precision; stable text/column placement matters more than a particular class.

## Layout

Design for the information and the user's task. The current sidebar, header, and scrolling
main region are a useful baseline. Navigation, sidebar width, scrolling behavior, and
breakpoints may evolve as fleet, machine, settings and investigation views become real.

All pages render inside one shared content container owned by `AppShell`: centered,
capped at 1500px, with the standard page padding — page code adds no extra max-width or
centering wrappers. Workspace pages (Processes) fill the same container's full height.
Card grids that may hold a single item use auto-fit tracks so one card spans the row
instead of stranding beside empty columns.
The scaffold uses 20px panel gaps/padding and 24px page padding. Treat these as comfortable
defaults. Add compact/comfortable density, smaller mobile gutters, wider investigation
layouts, or other spacing when appropriate. Reuse shared spacing choices once established.

Tables, lists, charts, grouped sections, toolbars, and split views are valid building blocks.
A fleet table does not need a ring-gauge card for every machine, and every region does not
need a bordered panel. Use grouping and alignment before adding containers. Keep reading
order, reachable controls and critical status clear across screen sizes. Horizontal
scrolling can be appropriate for a dense table; contain it within that surface.

## Elevation & Depth

Prefer the existing quiet surfaces and subtle borders as the default. Tonal differences,
stronger dividers, subtle shadows, or a combination can establish hierarchy. Menus, dialogs,
popovers, and other overlays may need elevation to remain distinct from the data behind them.

There is no global ban on shadows, gradients, or additional layers. Use them selectively,
keep both themes readable, and avoid overwhelming chart marks or making ordinary data
panels appear interactive. Focus indicators can use the most visible accessible treatment.

## Shapes

The current 12px panel, 8px row, and 6px control radii provide a coherent starting family.
They are defaults, not mandatory dimensions. Tables can have square edges; compact tools,
chips, overlays, and new containers can choose shapes suited to their job.

Use circles for meaningful gauges/status marks where useful. Rings are an available
visualization, not a required signature for every metric. Choose charts and marks based
on what people need to compare.

## Components

### Existing foundations

`Panel`, `EmptyState`, `AppShell`, navigation, and theme toggle are reusable starting points.
Reuse them when they fit. Add variants or new components when that produces a clearer
interface; avoid forcing every view through `Panel` and `EmptyState`.

Current component dimensions and styles are examples of the shipped scaffold. A new
component should share the broader visual language and interaction conventions without
copying every measurement.

### Data and connection states

Separate first load, empty results, unsupported metrics, permission failure, stale samples,
collector disconnect, and hub disconnect. Keep last-known values visible when useful,
with an explicit timestamp/state. A missing metric must not look like successful zero usage.

Dashed placeholder rings belong to the current development scaffold. Future empty/loading
states can use contextual explanations, skeletons, compact inline states, and appropriate
next actions. Avoid presenting an unavailable control as a working action.

### Interactions and motion

Give controls understandable labels and relevant hover, focus, selected, disabled, loading,
and error feedback. Preserve keyboard access and accessible names when labels collapse.
Use familiar controls for filters, time ranges, enrollment, settings and confirmations.

Brief transitions can clarify changes and make the interface feel responsive. Existing
color transitions are a baseline; expansion, movement and other feedback are allowed when
useful. Honor reduced-motion preferences and keep frequent metric updates stable enough
for reading and selection.

Process tables fill the available workspace and preserve column proportions during live
updates and browser resizing. Minimum readable widths trigger scrolling inside the table;
the toolbar stays visible and table headers remain pinned while rows scroll. Drag column dividers to resize, use arrow keys on a focused
divider, or double-click to reset. View mode, per-table sorting and widths persist in the
browser; search, expanded applications, pause and machine selection last for the tab session.
Machine selection is shared across Fleet, Overview, History and Processes for the tab.
The 224px desktop sidebar keeps Fleet above a visible list of authorized machines,
with names and available tags. Selected machines use a blue surface, outlined row,
stronger name and check mark. Choosing a machine preserves the active machine view
and uses the shared tab-scoped selection. The list scrolls within a bounded height
so Overview/History/Processes and the separated Alerts, Connections and admin-only
Administration links remain reachable. On the 56px mobile rail, machine initials and
view icons retain native titles and accessible names; selection remains outlined.
The logo and live connection footer retain their existing roles. Dark surfaces use
the charcoal website palette, brighter text, blue CPU, green memory and amber swap;
light tokens retain their established palette. Warning and critical meanings remain
separate, and dark keyboard focus uses a solid blue outline. Header
breadcrumbs show Fleet / machine / view; page-level tabs and pickers are omitted. Fleet
rows show compact CPU/memory trends received since sign-in. Overview combines CPU/GPU
and RAM/VRAM into two charts in one fixed ten-minute panel. History owns range and metric
selection for detailed investigation. Its picker groups CPU, memory, GPU, storage and
network controls, keeping core/card selection inside the relevant group. Global Select all / Unselect all share the Metrics disclosure row;
per-resource All / None actions update metric selection in one operation, respecting
machine capabilities and the hub's recorded metric catalog. Automatic history polling
leaves Refresh visually unchanged; manual refresh alone disables and animates it until
the request settles. Resource
sections fill the content width; related charts share a row on desktop and stack on
phones. CPU usage, I/O wait and CPU pressure share a percentage chart, while temperature
and power retain separate units. RAM and filesystem capacity each use one chart with
a Bytes / % switch when both stored metrics are selected; unit choices persist per
machine and visible exports follow the displayed unit. Swap belongs in the Memory
section. Recent usage legends number GPU cards to distinguish identical models. Collector diagnostics remain behind a bounded disclosure;
obvious explanatory copy is omitted, including on sign-in.

Live sampling, process updates and visible-tab history refreshes default to one second.
Refresh requests never overlap or replace existing charts with loading panels. Process
freshness changes update the status indicator without adding a banner or moving the table.

Per-core History defaults to all available cores, with an explicit All cores / None
and a scrollable checkbox picker for custom subsets. Up to eight series use lines;
larger selections use a scrollable heatmap of stored bucket peaks, with latest/peak
values and optional busiest-first order. Missing readings remain unfilled; recorded
gaps mask the heatmap. Requests allow the collector contract's 1024 cores while
retaining the existing scan/time budgets and partial-result indicators.

History retains its time range and per-machine metric, GPU-card and series visibility
choices in the browser; its machine, control expansion and auto-refresh state last for the tab.
Returning to Processes keeps the last snapshot visible while fresh data is requested.
Snapshots stay in memory, are scoped to the browser login, and show their freshness state.

## Do's and Don'ts

### Do:
- **Do** choose the layout and density that help people answer the monitoring question.
- **Do** reuse or extend shared tokens/components when the behavior or role repeats.
- **Do** preserve units, freshness, explicit states, and consistent metric meanings.
- **Do** verify keyboard behavior, zoom, contrast, and both themes when changing UI.
- **Do** evolve this guidance when a better reusable pattern emerges.

### Don't:
- **Don't** treat scaffold dimensions or examples as permanent product requirements.
- **Don't** force every screen into the same cards, chart types, or scrolling structure.
- **Don't** rely on color alone for status, selection, or chart interpretation.
- **Don't** show stale/unavailable data as healthy live measurements or fake interactivity.
- **Don't** sacrifice legibility or access to controls to preserve a visual convention.

## Release setup

Rigscope retains the established console styling. Fleet and Connections expose an
admin-only Add machine action that links to the collector enrollment section. Generated
commands use the current hub origin, and prompt privately for the separate, one-time
grant. Copy controls have visible status and a manual-selection fallback. Enrollment
adds no live-state banners above monitoring data.

## Product and identity

Rigscope is a self-hosted Linux resource/process monitor for a machine owner or small-fleet operator. Each collector connects outbound to independently operated hubs; each hub owns its users, permissions and history. The main tasks are spotting stale/unhealthy machines, inspecting resource history and opening live process detail. Remote mutating process actions are outside this release.

App surfaces use Operate mode: precise data, quiet surfaces, stable live updates and familiar controls. Freshness changes update existing status indicators without inserting banners above data. Interface copy consists of labels, values and actionable errors; omit redundant explanatory paragraphs.

The identity is an open rack frame around a rising trace and two sample points. `BrandMark` is the shared, decorative 24-unit SVG in the sidebar and sign-in row; the neighboring Rigscope text/link provides its accessible name. The app icon uses the same geometry with a dark backing and bright blue stroke for light/dark browser chrome. It is legible at 16–32px, with no animation, raster assets or layout changes. Keep this small brand gesture separate from metric semantics.

Machine rows use a fixed 16px status column and the same 12px gap and horizontal inset as navigation links, so every desktop label aligns. The status dot is green for fresh live samples, amber for stale or unavailable feeds, red for disconnected collectors, and muted before the first sample; the accessible name includes the status. CPU model appears below the machine name when available, with tags as the fallback. Mobile initials retain a small status dot.
