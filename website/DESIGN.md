# Rigscope website

The centered marketing page introduces Rigscope with installation as its primary
 action. It retains the product mark, dark charcoal palette, with self-hosted Geist variable type for the marketing page.
The document scrolls on desktop and mobile. The demo has a bounded height and scrolls internally; explanatory product sections follow it before the footer. Benefits and install controls share a centered alignment.
The compact introduction uses the descriptive product title and leads directly
to the wide demo. Benefits follow the demo on desktop and mobile. The desktop
demo uses an 18px rem base and a taller viewport for easier reading. The install command wraps on narrow screens,
with copy status and manual selection fallback. A compact four-step quick start
(install, sign in, add a machine, upgrade) and a measured light-footprint summary
follow the product sections.

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

The install controls are two stacked, labeled code boxes: normal user installation above system-wide installation. Each has its own 44px icon-only copy button inside its right edge and independent copy feedback. Preserve command tokens when wrapping on mobile. Geist is self-hosted under OFL 1.1 with its copyright and license included; deployed third-party notices are generated at build time.


Public documentation and guides share the landing page palette, Geist type,
brand, header and footer. Desktop reading pages have a left navigation column
and a prose column capped at 72ch. On mobile navigation wraps above the article.
Code blocks and tables scroll horizontally within the prose column. References
are generated from repository Markdown; guides live under website/content.
