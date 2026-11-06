"use client";

import { useEffect, useRef } from "react";
import { Menu, X } from "lucide-react";
import type { MachineViewKind } from "@/live/machine-selection";
import { Sidebar } from "./sidebar";

/** The same navigation in a modal drawer; the browser manages focus and Escape. */
export function MobileNavigation(props: { machineId?: string; machineView?: MachineViewKind }) {
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const desktop = window.matchMedia("(min-width: 768px)");
    const closeOnDesktop = () => { if (desktop.matches) dialog.current?.close(); };
    desktop.addEventListener("change", closeOnDesktop);
    return () => desktop.removeEventListener("change", closeOnDesktop);
  }, []);
  return <>
    <button type="button" aria-label="Open navigation" aria-haspopup="dialog"
      className="flex h-11 w-11 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-card-strong md:hidden"
      onClick={() => dialog.current?.showModal()}><Menu className="h-5 w-5" aria-hidden /></button>
    <dialog ref={dialog} aria-label="Navigation"
      className="fixed inset-y-0 left-0 m-0 h-dvh max-h-none w-[min(20rem,90vw)] max-w-none border-r border-border bg-background p-0 text-foreground backdrop:bg-black/50"
      onClick={(event) => { if (event.target === event.currentTarget) dialog.current?.close(); }}>
      <div className="flex h-full flex-col">
        <button type="button" aria-label="Close navigation" autoFocus
          className="absolute right-2 top-1.5 z-10 flex h-11 w-11 items-center justify-center rounded-md hover:bg-card-strong"
          onClick={() => dialog.current?.close()}><X className="h-5 w-5" aria-hidden /></button>
        <div className="flex min-h-0 flex-1" onClick={(event) => {
          if ((event.target as Element).closest("a, nav button")) dialog.current?.close();
        }}><Sidebar {...props} expanded /></div>
      </div>
    </dialog>
  </>;
}
