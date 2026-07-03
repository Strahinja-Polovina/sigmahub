"use client";

import * as React from "react";
import Link from "next/link";
import { Menu } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetTrigger,
  SheetClose,
} from "@/components/ui/sheet";
import { Logo } from "@/components/logo";
import { ThemeToggle } from "@/components/theme-toggle";
import { NAV_LINKS } from "@/components/marketing/content";

export function SiteNav() {
  const [open, setOpen] = React.useState(false);

  return (
    <header className="sticky top-0 z-50 border-b border-border/80 bg-background/80 backdrop-blur supports-backdrop-filter:bg-background/60">
      <div className="mx-auto flex h-14 max-w-6xl items-center justify-between gap-4 px-4 sm:px-6">
        <Link href="/" aria-label="SigmaHub home" className="shrink-0">
          <Logo />
        </Link>

        <nav className="hidden items-center gap-1 md:flex">
          {NAV_LINKS.map((link) => (
            <Button
              key={link.href}
              variant="ghost"
              size="sm"
              nativeButton={false}
              render={<Link href={link.href} />}
            >
              {link.label}
            </Button>
          ))}
        </nav>

        <div className="hidden items-center gap-1.5 md:flex">
          <Button
            variant="ghost"
            size="sm"
            nativeButton={false}
            render={<Link href="/login" />}
          >
            Log in
          </Button>
          <Button
            size="sm"
            nativeButton={false}
            render={<Link href="/dashboard" />}
          >
            Start free
          </Button>
          <ThemeToggle />
        </div>

        <div className="flex items-center gap-1 md:hidden">
          <ThemeToggle />
          <Sheet open={open} onOpenChange={setOpen}>
            <SheetTrigger
              render={
                <Button variant="ghost" size="icon" aria-label="Open menu" />
              }
            >
              <Menu />
            </SheetTrigger>
            <SheetContent side="right" className="w-72 gap-0 p-0">
              <div className="flex h-14 items-center border-b border-border px-4">
                <Logo />
              </div>
              <nav className="flex flex-col p-2">
                {NAV_LINKS.map((link) => (
                  <SheetClose
                    key={link.href}
                    render={
                      <Link
                        href={link.href}
                        className="rounded-md px-3 py-2.5 text-sm font-medium text-foreground hover:bg-muted"
                      />
                    }
                  >
                    {link.label}
                  </SheetClose>
                ))}
              </nav>
              <div className="mt-auto flex flex-col gap-2 border-t border-border p-4">
                <Button
                  variant="outline"
                  nativeButton={false}
                  render={<Link href="/login" onClick={() => setOpen(false)} />}
                >
                  Log in
                </Button>
                <Button
                  nativeButton={false}
                  render={
                    <Link href="/dashboard" onClick={() => setOpen(false)} />
                  }
                >
                  Start free
                </Button>
              </div>
            </SheetContent>
          </Sheet>
        </div>
      </div>
    </header>
  );
}
