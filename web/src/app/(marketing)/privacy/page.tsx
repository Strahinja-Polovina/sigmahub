import type { Metadata } from "next";
import Link from "next/link";

import {
  Bullets,
  Clause,
  LEGAL_ENTITY,
  LegalDocument,
} from "@/components/marketing/legal";

export const metadata: Metadata = {
  title: "Privacy Policy — SigmaHub",
  description:
    "What personal data SigmaHub processes, why, how long it is kept and how to have it erased.",
};

export default function PrivacyPage() {
  return (
    <LegalDocument slug="/privacy">
      <Clause heading="1. Who this covers">
        <p>
          This policy describes how {LEGAL_ENTITY.tradingName} processes personal
          data in two distinct roles, which carry different obligations and are
          deliberately kept apart throughout this document.
        </p>
        <Bullets
          items={[
            <>
              <strong>As controller</strong> — for the account and billing data of
              the people who sign up for SigmaHub, and for the operational
              telemetry the control plane needs in order to run.
            </>,
            <>
              <strong>As processor</strong> — for any personal data that happens
              to be inside the workloads you deploy: your database rows, your
              object storage, your backups, and the log lines your application
              writes. SigmaHub does not decide what goes in there; you do. Those
              are governed by the{" "}
              <Link href="/dpa">Data Processing Agreement</Link>, not by this
              policy.
            </>,
          ]}
        />
      </Clause>

      <Clause heading="2. What we collect as controller">
        <Bullets
          items={[
            <>
              <strong>Account</strong> — name, email address, hashed password or
              the identifier returned by your SSO provider, organisation name and
              membership role, and invitations you send or accept.
            </>,
            <>
              <strong>Billing</strong> — billing contact details, address, tax
              identifiers and subscription state. Payments run through Paddle as
              merchant of record; card numbers never reach SigmaHub.
            </>,
            <>
              <strong>Infrastructure metadata</strong> — the hostnames, IP
              addresses, distribution, CPU/RAM/disk and GPU inventory of the
              servers you connect, and the resources, domains and repositories
              you configure on them.
            </>,
            <>
              <strong>Operational telemetry</strong> — host and container
              metrics, deployment and build records, and the standard output and
              standard error of the containers you run, forwarded by the agent so
              you can read them in the dashboard.
            </>,
            <>
              <strong>Audit log</strong> — who did what and when, including every
              release of an encrypted secret.
            </>,
            <>
              <strong>Secrets you store</strong> — environment variables, backup
              target credentials and provider tokens. These are held envelope
              encrypted (see <Link href="/security">Security</Link>) and are
              never written to logs.
            </>,
          ]}
        />
        <p>
          <strong>Container logs deserve a specific warning.</strong> SigmaHub
          forwards your containers&rsquo; output verbatim. If your application
          logs an end user&rsquo;s email address, IP address or identifier on
          every request, that personal data is shipped to the control plane and
          stored there. Reduce what you log at the source where you can; the
          retention limits below bound what remains.
        </p>
      </Clause>

      <Clause heading="3. Why we process it">
        <Bullets
          items={[
            <>
              <strong>To perform the contract</strong> — creating your account,
              operating the control plane, deploying and supervising your
              resources, and billing you.
            </>,
            <>
              <strong>Legitimate interests</strong> — keeping the service secure
              and available, investigating abuse, and diagnosing failures. The
              audit log and the metrics exist for this reason.
            </>,
            <>
              <strong>Legal obligation</strong> — retaining invoices and tax
              records for the period the applicable tax law requires.
            </>,
          ]}
        />
        <p>
          We do not sell personal data, we do not use it to train models, and we
          do not run advertising or cross-site tracking on the product.
        </p>
      </Clause>

      <Clause heading="4. How long we keep it">
        <Bullets
          items={[
            <>
              <strong>Account and organisation data</strong> — for as long as the
              account exists, then deleted on teardown (clause 6).
            </>,
            <>
              <strong>Container logs</strong> — retained for a bounded window
              configured by the operator of your control plane, after which the
              log store expires them automatically. Logs are stored per tenant so
              an organisation&rsquo;s lines can be deleted without touching
              anyone else&rsquo;s.
            </>,
            <>
              <strong>Metrics</strong> — retained for the operator&rsquo;s
              configured metrics retention window; host samples held in the
              control-plane database are pruned on a shorter window still.
            </>,
            <>
              <strong>Audit log</strong> — kept for the life of the organisation,
              because its value is precisely that it cannot be quietly trimmed.
              It is deleted with the organisation.
            </>,
            <>
              <strong>Invoices</strong> — kept for the statutory period even after
              the account is deleted. This is the one category erasure does not
              reach, and the law is the reason.
            </>,
          ]}
        />
      </Clause>

      <Clause heading="5. Who else sees it">
        <p>
          The current sub-processor list, what each one receives and whether it
          applies to you at all is published in the{" "}
          <Link href="/dpa">Data Processing Agreement</Link>. Integrations are
          opt-in per organisation: connect no Git provider, no DNS provider and
          no alert channel, and none of those rows apply to you.
        </p>
        <p>
          Your own servers are not sub-processors. SigmaHub never sells, rents or
          resells servers — the machines remain yours, under your contract with
          your provider.
        </p>
      </Clause>

      <Clause heading="6. Your rights, and how erasure actually works">
        <p>
          Where the GDPR applies you may request access, rectification, erasure,
          restriction, portability, and object to processing based on legitimate
          interests. Write to {LEGAL_ENTITY.privacyEmail} and we will respond
          within one month.
        </p>
        <p>
          Erasure is a real operation here, not a promise. Tearing down an
          organisation deletes its rows in the control-plane database, issues a
          delete for that organisation&rsquo;s log lines in the log store and for
          its metric series in the metrics store, and revokes the credentials its
          agents were using. Data that has already left for a sub-processor
          — an invoice at Paddle — is deleted according to that
          sub-processor&rsquo;s own obligations and the statutory retention
          above.
        </p>
        <p>
          You also have the right to lodge a complaint with your supervisory
          authority.
        </p>
      </Clause>

      <Clause heading="7. International transfers">
        <p>
          Where a sub-processor listed in the DPA processes data outside the
          EEA/UK, the transfer relies on the European Commission&rsquo;s Standard
          Contractual Clauses (and the UK Addendum where applicable) together
          with the supplementary measures described in{" "}
          <Link href="/security">Security</Link>. The region the control plane
          itself runs in is set by the operator; ask before onboarding if it
          matters to you.
        </p>
      </Clause>

      <Clause heading="8. Cookies">
        <p>
          The dashboard sets a session cookie for authentication and stores your
          theme preference. Both are strictly necessary for the product to work.
          There are no analytics, advertising or third-party tracking cookies on
          the marketing site or in the product.
        </p>
      </Clause>

      <Clause heading="9. Changes and contact">
        <p>
          Material changes will be announced to account owners before they take
          effect, and the effective date at the top of this page will move.
          Privacy questions: {LEGAL_ENTITY.privacyEmail}. Data protection
          enquiries and DPA requests: {LEGAL_ENTITY.dpoEmail}. Security reports:{" "}
          {LEGAL_ENTITY.securityEmail}.
        </p>
      </Clause>
    </LegalDocument>
  );
}
