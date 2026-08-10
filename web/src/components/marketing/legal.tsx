/**
 * Legal document chrome and the facts every legal page shares.
 *
 * SIGMA-297. The marketing footer advertised Privacy, Terms, Security and DPA
 * and pointed all four at "#". That is not a cosmetic gap:
 *
 *   - Paddle is the merchant of record for SigmaHub's billing. Paddle's seller
 *     review requires reachable Terms of Service and Privacy Policy URLs before
 *     an account leaves sandbox, so `CP_PADDLE_ENV=production` is unreachable
 *     while those links go nowhere.
 *   - A design partner's DPO clicking "DPA" during diligence landed back on the
 *     homepage. For a product whose agent runs as root on the customer's hosts
 *     and whose control plane stores their secrets, "there is no DPA" ends the
 *     evaluation.
 *
 * Rules for editing anything in this file:
 *
 *   1. Every factual claim about how the system works must be true of the code
 *      in this repository. The security page in particular is read as a
 *      representation — if a control is on the roadmap, say roadmap. The
 *      compliance line matches the marketing FAQ deliberately: SOC 2 / GDPR is
 *      a ROADMAP, SigmaHub is not certified, and neither page may drift into
 *      implying otherwise.
 *   2. Do not invent corporate identity. The registered company name, number,
 *      address and governing-law seat are the one set of facts this repository
 *      cannot know; they live in LEGAL_ENTITY below as a single explicit block
 *      so there is exactly one place to complete before these documents are
 *      relied on commercially. Until it is completed the pages say the details
 *      are available on request rather than printing a fabricated address —
 *      a wrong registered address in a DPA is worse than none.
 */

import type { ReactNode } from "react";
import Link from "next/link";

import { Container } from "@/components/marketing/primitives";
import { cn } from "@/lib/utils";

/* -------------------------------------------------------------------------- */
/*  Shared facts                                                              */
/* -------------------------------------------------------------------------- */

/**
 * The operating entity. `tradingName` and the contact addresses follow the
 * repository's own canonical domain (sigmahub.io, as used across the marketing
 * copy and the product mock). `registeredDetails` is deliberately null: fill in
 * the registered name, company number, seat and governing law before signing a
 * DPA or submitting the site to Paddle's seller review, and the pages will
 * render them in place of the "available on request" fallback.
 */
export const LEGAL_ENTITY = {
  tradingName: "SigmaHub",
  registeredDetails: null as string | null,
  privacyEmail: "privacy@sigmahub.io",
  securityEmail: "security@sigmahub.io",
  legalEmail: "legal@sigmahub.io",
  dpoEmail: "dpo@sigmahub.io",
} as const;

/** Effective date printed on every document. Bump when the text changes. */
export const LEGAL_EFFECTIVE_DATE = "10 August 2026";

/**
 * The four documents, in the order the footer lists them. The footer builds its
 * Legal column from this array, which is what keeps the links and the routes
 * from drifting apart again: adding a document here without adding the route
 * fails `site-footer.test.tsx`.
 */
export const LEGAL_DOCS = [
  {
    slug: "/privacy",
    label: "Privacy",
    title: "Privacy Policy",
    summary:
      "What personal data SigmaHub processes, why, how long it is kept and how to have it erased.",
  },
  {
    slug: "/terms",
    label: "Terms",
    title: "Terms of Service",
    summary:
      "The agreement between you and SigmaHub for use of the control plane, the agent and the dashboard.",
  },
  {
    slug: "/security",
    label: "Security",
    title: "Security",
    summary:
      "How the control plane, the mesh, the agent and your secrets are protected — and what is roadmap, not shipped.",
  },
  {
    slug: "/dpa",
    label: "DPA",
    title: "Data Processing Agreement",
    summary:
      "The Article 28 terms under which SigmaHub processes personal data on your behalf, including the sub-processor list.",
  },
] as const;

/**
 * Sub-processors. Only parties that can actually see customer data belong here.
 *
 * Two things make this list shorter than a typical SaaS list, and both are
 * real properties of the product rather than marketing: SigmaHub never rents or
 * resells servers, so the customer's own hosts and their provider are the
 * customer's own controllers, not our sub-processors; and the integrations are
 * opt-in per organisation, so a customer who connects nothing has only the
 * always-on rows applying to them.
 */
export const SUB_PROCESSORS = [
  {
    name: "Paddle.com Market Ltd",
    purpose:
      "Merchant of record: subscription billing, invoicing, payment processing and sales-tax compliance.",
    data: "Billing contact name and email, billing address, tax identifiers, subscription and usage quantities. SigmaHub never receives or stores card numbers.",
    scope: "Always, for paid organisations",
  },
  {
    name: "Control-plane hosting provider",
    purpose:
      "Hosts the control plane, its Postgres database and the telemetry store (VictoriaMetrics and Loki).",
    data: "All data described in the Privacy Policy, at rest on the operator's infrastructure.",
    scope: "Always",
  },
  {
    name: "GitHub, Inc. / GitLab B.V.",
    purpose:
      "Source of the repositories you deploy from, and destination of the deploy-status checks written back.",
    data: "Repository metadata, commit SHAs and authors, installation identifiers, deployment status.",
    scope: "Only if you connect a Git provider",
  },
  {
    name: "Cloudflare, Inc.",
    purpose: "DNS record management for domains you attach to a resource.",
    data: "Domain names, DNS records, zone and account identifiers, the API credential you supply.",
    scope: "Only if you connect Cloudflare DNS",
  },
  {
    name: "Slack Technologies / Telegram FZ-LLC",
    purpose: "Delivery of the alert notifications you configure.",
    data: "Alert text, which names the affected server or resource and the condition that fired.",
    scope: "Only if you configure that alert channel",
  },
  {
    name: "Hugging Face, Inc.",
    purpose: "Model catalogue lookups and model artefact downloads for GPU resources.",
    data: "Model identifiers requested. No customer personal data is sent.",
    scope: "Only if you deploy a model-hosting resource",
  },
] as const;

/* -------------------------------------------------------------------------- */
/*  Page chrome                                                               */
/* -------------------------------------------------------------------------- */

/**
 * The shell every legal page renders into: title, effective date, a summary
 * line, and cross-links to the sibling documents so a reviewer who arrived at
 * one of them can reach the other three without going back to the footer.
 */
export function LegalDocument({
  slug,
  children,
}: {
  slug: (typeof LEGAL_DOCS)[number]["slug"];
  children: ReactNode;
}) {
  const doc = LEGAL_DOCS.find((d) => d.slug === slug);
  if (!doc) throw new Error(`unknown legal document ${slug}`);

  return (
    <div className="py-16 sm:py-20">
      <Container className="max-w-3xl">
        <p className="font-mono text-xs uppercase tracking-wider text-muted-foreground">
          Legal
        </p>
        <h1 className="mt-3 text-3xl font-semibold tracking-tight text-foreground sm:text-4xl">
          {doc.title}
        </h1>
        <p className="mt-4 text-base leading-relaxed text-muted-foreground">
          {doc.summary}
        </p>
        <p className="mt-4 font-mono text-xs text-muted-foreground">
          Effective {LEGAL_EFFECTIVE_DATE}
        </p>

        <nav
          aria-label="Legal documents"
          className="mt-8 flex flex-wrap gap-2 border-y border-border py-4"
        >
          {LEGAL_DOCS.map((d) => (
            <Link
              key={d.slug}
              href={d.slug}
              aria-current={d.slug === slug ? "page" : undefined}
              className={cn(
                "rounded-md px-3 py-1.5 text-sm transition-colors",
                d.slug === slug
                  ? "bg-muted font-medium text-foreground"
                  : "text-muted-foreground hover:bg-muted/60 hover:text-foreground",
              )}
            >
              {d.label}
            </Link>
          ))}
        </nav>

        <div className="mt-10 space-y-10">{children}</div>

        <p className="mt-14 border-t border-border pt-6 text-sm leading-relaxed text-muted-foreground">
          {LEGAL_ENTITY.registeredDetails ??
            `Registration details for the ${LEGAL_ENTITY.tradingName} operating entity are available on request from ${LEGAL_ENTITY.legalEmail}.`}
        </p>
      </Container>
    </div>
  );
}

/** A numbered clause block. */
export function Clause({
  id,
  heading,
  children,
}: {
  id?: string;
  heading: string;
  children: ReactNode;
}) {
  return (
    <section id={id} className="scroll-mt-20">
      <h2 className="text-xl font-semibold tracking-tight text-foreground">
        {heading}
      </h2>
      <div className="mt-3 space-y-3 text-base leading-relaxed text-muted-foreground [&_a]:text-foreground [&_a]:underline [&_a]:underline-offset-4 [&_strong]:font-medium [&_strong]:text-foreground">
        {children}
      </div>
    </section>
  );
}

/** Bulleted list with the document's list rhythm. */
export function Bullets({ items }: { items: ReactNode[] }) {
  return (
    <ul className="list-disc space-y-2 pl-5">
      {items.map((item, i) => (
        <li key={i}>{item}</li>
      ))}
    </ul>
  );
}
