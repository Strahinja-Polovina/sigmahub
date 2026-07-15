import type { Metadata } from "next";
// Self-hosted variable fonts (see globals.css). Vendored via @fontsource so
// builds don't depend on Google Fonts egress at build time.
import "@fontsource-variable/inter";
import "@fontsource-variable/jetbrains-mono";
import "./globals.css";
import { ThemeProvider } from "@/components/theme-provider";
import { TooltipProvider } from "@/components/ui/tooltip";
import { Toaster } from "@/components/ui/sonner";

export const metadata: Metadata = {
  title: "SigmaHub — Managed cloud PaaS for your own servers",
  description:
    "Run apps, databases, object storage, GPU/LLM inference, backups, disaster recovery and Kubernetes on servers you own — from one cloud dashboard.",
};

export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en" suppressHydrationWarning className="h-full antialiased">
      <body className="min-h-full">
        {/* Without JS, reveal-on-scroll wrappers would stay hidden — force them visible. */}
        <noscript>
          <style>{`[data-shown="false"]{opacity:1!important;transform:none!important}`}</style>
        </noscript>
        <ThemeProvider
          attribute="class"
          defaultTheme="light"
          enableSystem={false}
          disableTransitionOnChange
        >
          <TooltipProvider delay={200}>
            {children}
            <Toaster richColors position="top-right" />
          </TooltipProvider>
        </ThemeProvider>
      </body>
    </html>
  );
}
