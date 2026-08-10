// @vitest-environment jsdom
//
// SIGMA-297. The footer used to render a "Legal" column whose four links —
// Privacy, Terms, Security, DPA — all pointed at "#", plus a "Company" column
// that did the same. A bare "#" is not a link: it scrolls nowhere and leaves
// the visitor on the page they were already on. That is merely sloppy for
// "Careers"; for "DPA" it is a factual claim that a data processing agreement
// exists, made to someone doing security diligence on a product that runs an
// agent as root on their hosts. It also blocks Paddle, which requires
// reachable Terms and Privacy URLs before a seller account leaves sandbox.
//
// This test is the standing guarantee: every link the footer renders either
// points at a real app-router route or at a fragment that is not empty.

import { describe, expect, it } from "vitest";
import { readdirSync, existsSync } from "node:fs";
import path from "node:path";
import { render } from "@testing-library/react";

import { SiteFooter } from "./site-footer";

const APP_DIR = path.resolve(__dirname, "../../app");

/**
 * Walk the app router directory and collect every route a `page.tsx` defines.
 * Route groups — directories wrapped in parentheses, e.g. `(marketing)` — do
 * not contribute a path segment, which is exactly why a link like "/privacy"
 * can be served by `src/app/(marketing)/privacy/page.tsx`.
 */
function collectRoutes(dir: string, prefix = ""): string[] {
  const routes: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (entry.isFile() && /^page\.(tsx|ts|jsx|js)$/.test(entry.name)) {
      routes.push(prefix === "" ? "/" : prefix);
      continue;
    }
    if (!entry.isDirectory()) continue;
    // Private folders (_foo) and the API tree never produce pages.
    if (entry.name.startsWith("_") || entry.name === "api") continue;
    const isGroup = entry.name.startsWith("(") && entry.name.endsWith(")");
    routes.push(
      ...collectRoutes(
        path.join(dir, entry.name),
        isGroup ? prefix : `${prefix}/${entry.name}`
      )
    );
  }
  return routes;
}

describe("SiteFooter", () => {
  it("every footer link resolves to a route", () => {
    expect(existsSync(APP_DIR)).toBe(true);
    const routes = new Set(collectRoutes(APP_DIR));

    const { container } = render(<SiteFooter />);
    const anchors = Array.from(container.querySelectorAll("a"));
    expect(anchors.length).toBeGreaterThan(0);

    const dead: string[] = [];
    const unresolved: string[] = [];

    for (const a of anchors) {
      // jsdom resolves href against the document base, so read the attribute.
      const href = a.getAttribute("href") ?? "";
      const label = a.textContent?.trim() ?? "(no label)";

      if (href === "#" || href === "") {
        dead.push(`${label} -> ${JSON.stringify(href)}`);
        continue;
      }
      // A fragment link is an on-page anchor; the marketing page renders those
      // section ids, so anything non-empty after "#" is legitimate.
      if (href.startsWith("#")) continue;
      // External links are out of scope for the router manifest check.
      if (/^[a-z]+:/i.test(href)) continue;

      const routePath = href.split(/[?#]/)[0].replace(/\/$/, "") || "/";
      if (!routes.has(routePath)) unresolved.push(`${label} -> ${href}`);
    }

    expect(dead, `footer links pointing nowhere: ${dead.join(", ")}`).toEqual([]);
    expect(
      unresolved,
      `footer links with no app-router page: ${unresolved.join(", ")}`
    ).toEqual([]);
  });

  it("publishes the four legal documents Paddle and a DPO ask for", () => {
    const routes = new Set(collectRoutes(APP_DIR));
    for (const route of ["/privacy", "/terms", "/security", "/dpa"]) {
      expect(routes.has(route), `missing route ${route}`).toBe(true);
    }

    const { container } = render(<SiteFooter />);
    const hrefs = Array.from(container.querySelectorAll("a")).map((a) =>
      a.getAttribute("href")
    );
    for (const route of ["/privacy", "/terms", "/security", "/dpa"]) {
      expect(hrefs, `footer does not link ${route}`).toContain(route);
    }
  });
});
