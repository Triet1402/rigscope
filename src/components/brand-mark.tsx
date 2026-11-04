import type { SVGProps } from "react";

/** A rack frame and rising trace, drawn on a 24px grid for small sizes. */
export function BrandMark(props: SVGProps<SVGSVGElement>) {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false" {...props}>
      <path d="M8 3H4v18h4M16 3h4v18h-4" />
      <path d="m8 15 4-6 4 3M8 8h.01M16 17h.01" />
    </svg>
  );
}
