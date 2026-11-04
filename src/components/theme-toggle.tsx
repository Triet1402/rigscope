"use client";

import { Moon, Sun } from "lucide-react";
import { useTheme } from "next-themes";

export function ThemeToggle() {
  const { resolvedTheme, setTheme } = useTheme();

  // resolvedTheme never flickers after hydration; render a stable button.
  return (
    <button
      type="button"
      onClick={() => setTheme(resolvedTheme === "dark" ? "light" : "dark")}
      aria-label="Toggle theme"
      className="flex h-9 w-9 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-selected hover:text-foreground"
    >
      {/* Both icons rendered; CSS picks the right one, so no hydration mismatch. */}
      <Sun className="h-4.5 w-4.5 dark:hidden" />
      <Moon className="hidden h-4.5 w-4.5 dark:block" />
    </button>
  );
}
