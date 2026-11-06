# Rigscope website

The centered marketing page introduces Rigscope with installation as its primary
 action. It retains the product mark, dark charcoal palette, with self-hosted Geist variable type for the marketing page.
Desktop fills the viewport with a flexible demo and footer anchored below it; the embedded product scrolls internally. On mobile the document scrolls normally. Benefits and install controls share a centered alignment.
On desktop, benefits precede the wide demo; on mobile, the demo follows the install
command and benefits come afterward. The install command wraps on narrow screens,
with copy status and manual selection fallback.

The product demo directly imports the real Fleet, Overview, History and Processes
views, their shared shell/sidebar and root stylesheet. There is no separate demo
component vocabulary. Root DESIGN.md owns this UI and its theme. Sidebar changes
and theme tokens automatically carry into the demo on rebuild.

Only browser routing and live/history/process data are adapted in
src/product-demo/. All machines, readings and processes are synthetic, visibly
labeled, and require no live hub or account. A standalone iframe isolates product
styling and scrolling from the marketing page; the full demo link opens the same
document. Keep existing product responsiveness, keyboard behavior, labels, charts,
process aggregation and table controls intact.

Alerts, Connections and Administration retain their place after machine views;
the public demo explains that configuration is available in an installation.
Contact links to the maintainer's GitHub profile. No generated images ship.

The install control is one centered code box with a 44px icon-only copy button inside its right edge. Preserve command tokens when wrapping on mobile. Geist is self-hosted under OFL 1.1 with its copyright and license included; deployed third-party notices are generated at build time.
