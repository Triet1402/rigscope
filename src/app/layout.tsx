import type { Metadata } from "next";
import { ThemeProvider } from "@/components/theme-provider";
import { LiveProvider } from "@/live/live-provider";
import "./globals.css";

export const metadata: Metadata = {
  title: {
    template: "%s — Rigscope",
    default: "Rigscope",
  },
  description:
    "Self-hosted remote system monitor — a live fleet of collectors behind one authenticated hub.",
};

export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    // suppressHydrationWarning: next-themes mutates <html> class before hydration.
    <html lang="en" suppressHydrationWarning>
      <body>
        {/* dark is the default (matches the intended rendering environment); system is respected. */}
        <ThemeProvider attribute="class" defaultTheme="dark" enableSystem>
          {/* One shared session + live stream for the whole tab; views only subscribe. */}
          <LiveProvider>{children}</LiveProvider>
        </ThemeProvider>
      </body>
    </html>
  );
}
