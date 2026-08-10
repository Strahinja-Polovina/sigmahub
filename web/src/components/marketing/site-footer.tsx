import * as React from "react";
import Link from "next/link";

import { Logo } from "@/components/logo";
import { FOOTER_COLUMNS } from "@/components/marketing/content";

export function SiteFooter() {
  return (
    <footer className="border-t border-border bg-muted/40">
      <div className="mx-auto max-w-6xl px-4 py-14 sm:px-6">
        {/* Brand block plus one equal track per footer column. The track count
            is derived from FOOTER_COLUMNS rather than hard-coded, so dropping a
            column — SIGMA-297 removed the placeholder "Company" one, whose four
            links all pointed at "#" — does not leave a dangling empty track. */}
        <div
          className="grid gap-10 sm:grid-cols-2 lg:[grid-template-columns:1.5fr_repeat(var(--footer-cols),1fr)]"
          style={{ "--footer-cols": FOOTER_COLUMNS.length } as React.CSSProperties}
        >
          <div className="max-w-xs">
            <Logo />
            <p className="mt-4 text-sm leading-relaxed text-muted-foreground">
              Managed cloud PaaS for servers you own. One dashboard, one bill —
              €5 per unit, everything included.
            </p>
          </div>

          {FOOTER_COLUMNS.map((col) => (
            <div key={col.heading}>
              <h3 className="font-mono text-xs font-medium uppercase tracking-wider text-muted-foreground">
                {col.heading}
              </h3>
              <ul className="mt-4 space-y-2.5">
                {col.links.map((link) => (
                  <li key={link.label}>
                    <Link
                      href={link.href}
                      className="text-sm text-foreground/80 transition-colors hover:text-foreground"
                    >
                      {link.label}
                    </Link>
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </div>

        <div className="mt-12 flex flex-col items-start justify-between gap-3 border-t border-border pt-6 text-xs text-muted-foreground sm:flex-row sm:items-center">
          <p>© {new Date().getFullYear()} SigmaHub. All rights reserved.</p>
          <p className="font-mono">
            Never resells servers · Your infrastructure, your control
          </p>
        </div>
      </div>
    </footer>
  );
}
