// @vitest-environment jsdom
//
// SIGMA-297. The routes existing is necessary but not sufficient — a page that
// throws at render is still a dead link to the DPO clicking it. These render
// each of the four documents and assert the load-bearing content is present:
// the sub-processor list Article 28(2) needs, the honest "not certified"
// compliance line, and the erasure path that the offboarding work depends on.

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";

import PrivacyPage from "@/app/(marketing)/privacy/page";
import TermsPage from "@/app/(marketing)/terms/page";
import SecurityPage from "@/app/(marketing)/security/page";
import DpaPage from "@/app/(marketing)/dpa/page";
import { LEGAL_DOCS, SUB_PROCESSORS } from "./legal";

afterEach(cleanup);

const PAGES = [
  ["/privacy", PrivacyPage],
  ["/terms", TermsPage],
  ["/security", SecurityPage],
  ["/dpa", DpaPage],
] as const;

describe("legal documents", () => {
  it.each(PAGES)("%s renders with its title and sibling nav", (slug, Page) => {
    render(<Page />);

    const doc = LEGAL_DOCS.find((d) => d.slug === slug);
    expect(doc).toBeDefined();
    expect(
      screen.getByRole("heading", { level: 1, name: doc!.title })
    ).toBeTruthy();

    // Every document reaches the other three, so a reviewer who landed on one
    // is never a dead end away from the rest.
    const nav = screen.getByRole("navigation", { name: /legal documents/i });
    for (const other of LEGAL_DOCS) {
      expect(
        within(nav).getByRole("link", { name: other.label }).getAttribute("href")
      ).toBe(other.slug);
    }
  });

  it("the DPA publishes the sub-processor list", () => {
    render(<DpaPage />);
    for (const sp of SUB_PROCESSORS) {
      expect(screen.getByText(sp.name)).toBeTruthy();
    }
    // Paddle is the one that gates leaving billing sandbox, and the one a
    // reviewer looks for first.
    expect(screen.getByText(/Paddle\.com Market Ltd/)).toBeTruthy();
    // The erasure mechanism must be described, not merely promised.
    expect(screen.getByText(/Per-tenant deletes are issued/)).toBeTruthy();
  });

  it("the security page states the compliance position without overclaiming", () => {
    const { container } = render(<SecurityPage />);
    const text = container.textContent ?? "";
    expect(text).toMatch(/not SOC 2 certified/i);
    expect(text).toMatch(/holds no ISO 27001 certificate/i);
    // Guard against a future edit quietly promoting the roadmap to a claim.
    expect(text).not.toMatch(/SOC 2 certified\b(?!.*not)/i);
  });
});
