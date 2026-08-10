import type { Metadata } from "next";
import Link from "next/link";

import {
  Bullets,
  Clause,
  LEGAL_ENTITY,
  LegalDocument,
} from "@/components/marketing/legal";

export const metadata: Metadata = {
  title: "Terms of Service — SigmaHub",
  description:
    "The agreement between you and SigmaHub for use of the control plane, the agent and the dashboard.",
};

export default function TermsPage() {
  return (
    <LegalDocument slug="/terms">
      <Clause heading="1. The agreement">
        <p>
          These terms govern your use of {LEGAL_ENTITY.tradingName} — the control
          plane, the dashboard and the <code>sigmad</code> agent. By creating an
          account or connecting a server you accept them. If you accept on behalf
          of an organisation, you confirm you are authorised to bind it.
        </p>
      </Clause>

      <Clause heading="2. What the service is — and is not">
        <p>
          SigmaHub is a managed control plane for servers <em>you</em> own or
          rent. It deploys and supervises workloads on those machines, manages
          databases, object storage, backups, DNS, TLS and a WireGuard mesh, and
          shows you the result in one dashboard.
        </p>
        <p>
          <strong>SigmaHub never sells, rents or resells servers</strong> and
          never marks up your infrastructure. Your contract with your hosting
          provider is yours; their outage is between you and them. We charge for
          the platform only.
        </p>
      </Clause>

      <Clause heading="3. Your servers, your authority, root access">
        <p>
          Onboarding a server installs an agent that runs with root privileges on
          that host. That is the level of access required to manage containers,
          the firewall, the mesh interface and system services — and it is the
          most important thing to understand before you connect a machine.
        </p>
        <Bullets
          items={[
            <>
              You represent that you are entitled to install software with root
              privileges on every host you connect, and that doing so breaches no
              agreement you have with anyone else.
            </>,
            <>
              You are responsible for what runs on your hosts and for the data
              your workloads store, including its lawfulness.
            </>,
            <>
              You keep your own credentials for your hosts. SigmaHub&rsquo;s
              agent is outbound-only and requires no inbound ports; it can be
              removed with the documented uninstall path, which is a supported
              operation and not a penalty.
            </>,
          ]}
        />
      </Clause>

      <Clause heading="4. Accounts and acceptable use">
        <p>
          Keep your credentials confidential and tell us promptly at{" "}
          {LEGAL_ENTITY.securityEmail} if you believe they have been
          compromised. You must not use SigmaHub to attack third-party systems,
          to distribute malware, to send unsolicited bulk mail, to host material
          you have no right to host, or to circumvent the platform&rsquo;s own
          limits and metering.
        </p>
      </Clause>

      <Clause heading="5. Fees, metering and billing">
        <Bullets
          items={[
            <>
              Pricing is <strong>&euro;5 per unit per month</strong>. A server
              counts as one unit if it is an ordinary Docker host, two if it is a
              Kubernetes node and four if it is a GPU host — the weighting
              reflects management cost, not the price of the machine.
            </>,
            <>
              The first three units are free. Every feature is included at every
              tier; there are no add-ons and no feature gates.
            </>,
            <>
              Billing is handled by Paddle.com Market Ltd as merchant of record.
              Paddle is the seller for your purchase, issues your invoice and
              collects any applicable sales tax; Paddle&rsquo;s buyer terms apply
              to the transaction itself.
            </>,
            <>
              Charges are based on metered units for the period. Disconnecting a
              server stops it counting from the point the control plane records
              the decommission.
            </>,
          ]}
        />
      </Clause>

      <Clause heading="6. Beta status and availability">
        <p>
          SigmaHub is offered without a contractual uptime commitment. There is
          no service level agreement, no service credits and no guarantee that a
          given release is free of defects. Where a feature is described as
          roadmap — on this site or in{" "}
          <Link href="/security">Security</Link> — it is not a present
          representation and must not be relied on in procurement.
        </p>
      </Clause>

      <Clause heading="7. Data protection">
        <p>
          Personal data we process as controller is described in the{" "}
          <Link href="/privacy">Privacy Policy</Link>. Personal data we process on
          your behalf is governed by the{" "}
          <Link href="/dpa">Data Processing Agreement</Link>, which forms part of
          these terms and takes precedence over them in the event of a conflict
          about processing.
        </p>
      </Clause>

      <Clause heading="8. Intellectual property">
        <p>
          SigmaHub retains all rights in the platform. You retain all rights in
          your code, your images, your configuration and your data — deploying
          them through SigmaHub grants us only the licence needed to operate the
          service for you. Any feedback you send may be used without obligation.
        </p>
      </Clause>

      <Clause heading="9. Warranties and liability">
        <p>
          The service is provided &ldquo;as is&rdquo; and, to the extent
          permitted by law, without warranties of merchantability, fitness for a
          particular purpose or non-infringement.
        </p>
        <p>
          Neither party is liable for indirect or consequential loss, or for lost
          profits, revenue or goodwill. Each party&rsquo;s aggregate liability is
          capped at the fees paid in the twelve months before the event giving
          rise to the claim. Nothing limits liability for death or personal
          injury caused by negligence, for fraud, or for anything else that
          cannot lawfully be limited.
        </p>
        <p>
          <strong>Backups.</strong> SigmaHub schedules, encrypts and verifies
          backups to targets you configure, but it is not your only copy of
          record. Test your restores.
        </p>
      </Clause>

      <Clause heading="10. Suspension and termination">
        <p>
          You may close your account at any time. We may suspend an account for
          non-payment or for a breach of clause 4, with notice where
          circumstances allow. On termination the organisation is torn down: its
          data is deleted as described in the{" "}
          <Link href="/privacy">Privacy Policy</Link>, including the deletion of
          its log lines and metric series. Export anything you need first.
          Uninstalling the agent leaves your workloads running on your hosts;
          they simply stop being managed.
        </p>
      </Clause>

      <Clause heading="11. Changes, governing law and contact">
        <p>
          We may change these terms; material changes will be announced to
          account owners before they take effect and the effective date above
          will move. These terms are governed by the law of the jurisdiction in
          which the {LEGAL_ENTITY.tradingName} operating entity is established,
          and its courts have exclusive jurisdiction, without prejudice to any
          mandatory consumer protections available where you live. Questions:{" "}
          {LEGAL_ENTITY.legalEmail}.
        </p>
      </Clause>
    </LegalDocument>
  );
}
