import type { Metadata } from "next";
import Link from "next/link";

import {
  Bullets,
  Clause,
  LEGAL_ENTITY,
  LegalDocument,
} from "@/components/marketing/legal";

export const metadata: Metadata = {
  title: "Security — SigmaHub",
  description:
    "How the control plane, the mesh, the agent and your secrets are protected — and what is roadmap, not shipped.",
};

export default function SecurityPage() {
  return (
    <LegalDocument slug="/security">
      <Clause heading="1. The trust boundary">
        <p>
          SigmaHub is unusual in that the machines are yours. The control plane
          runs on infrastructure the operator manages; the{" "}
          <code>sigmad</code> agent runs as root on your hosts, because managing
          containers, the firewall, the mesh interface and system services
          requires it. Everything below follows from that shape.
        </p>
      </Clause>

      <Clause heading="2. Network">
        <Bullets
          items={[
            <>
              <strong>Outbound-only agent.</strong> The agent dials the control
              plane. No inbound port is opened on your host for SigmaHub itself,
              which is why hosts behind NAT or CGNAT work without a public
              address.
            </>,
            <>
              <strong>WireGuard mesh.</strong> Control-plane-to-host traffic and
              host-to-host traffic run over a WireGuard mesh, so the control
              channel is not exposed on the public internet.
            </>,
            <>
              <strong>TLS at the edge.</strong> Public traffic to your workloads
              terminates at a per-host proxy with certificates obtained
              automatically over ACME and renewed without intervention.
            </>,
          ]}
        />
      </Clause>

      <Clause heading="3. Agent supply chain">
        <p>
          Agent binaries are signed and verified with <code>cosign</code> before
          installation and before any self-update; an artefact that fails
          verification is refused rather than run. The one artefact signature
          cannot cover is the bootstrap installer itself — it is the script that
          <em>runs</em> cosign — so the control plane serves it over HTTPS and
          the onboarding flow refuses to render a non-HTTPS install command.
        </p>
      </Clause>

      <Clause heading="4. Secrets">
        <Bullets
          items={[
            <>
              <strong>Envelope encryption.</strong> Environment variables, backup
              target credentials and provider tokens are encrypted with a
              per-object data key, which is itself wrapped by a master key held
              in key custody. The ciphertext and the wrapping key are separate
              concerns by construction.
            </>,
            <>
              <strong>AES-256-GCM</strong> throughout, with the key custody
              backend chosen by the operator: HashiCorp Vault Transit for
              production deployments, or a file-held master key for development.
              The file backend is documented as development-only precisely
              because the key sits on the same host as the ciphertext.
            </>,
            <>
              <strong>Audited unwraps.</strong> Every release of a secret goes
              through the audited path and lands in the audit log. Secret values
              are never written to application logs.
            </>,
            <>
              <strong>Write-only in the UI.</strong> Stored secret values are not
              read back to the dashboard; they can be replaced, not retrieved.
            </>,
          ]}
        />
      </Clause>

      <Clause heading="5. Tenancy, access and audit">
        <Bullets
          items={[
            <>
              Every object belongs to an organisation, and queries are scoped to
              the caller&rsquo;s organisation at the data layer rather than in
              the UI.
            </>,
            <>
              Roles govern who may connect servers, deploy, manage secrets and
              invite members. Invitations are single-use and expire.
            </>,
            <>
              Telemetry is stored per tenant, which is what makes a per-customer
              log and metric deletion possible on offboarding rather than a
              volume-wide wipe.
            </>,
            <>
              The audit log records who did what and when, including secret
              releases, and is retained for the life of the organisation.
            </>,
          ]}
        />
      </Clause>

      <Clause heading="6. Backups and recovery">
        <p>
          Backups are encrypted before they leave the host and are written to
          targets you own. Restores are verified rather than assumed, and
          point-in-time recovery is available for the supported database engines.
          SigmaHub is not a substitute for your own copy of record — see clause 9
          of the <Link href="/terms">Terms</Link>.
        </p>
      </Clause>

      <Clause heading="7. Compliance status — read this one carefully">
        <p>
          <strong>SigmaHub is not SOC 2 certified and holds no ISO 27001
          certificate.</strong> SOC 2 and formal GDPR assurance are on the
          roadmap. Anything you read on the marketing site describing compliance
          is a statement about that roadmap, not about a completed audit, and it
          must not be entered into a vendor questionnaire as a certification.
        </p>
        <p>
          What does exist today and can be relied on: the{" "}
          <Link href="/dpa">Data Processing Agreement</Link> with its
          sub-processor list, the technical measures described above, bounded
          telemetry retention, and a tenant offboarding path that actually
          deletes.
        </p>
      </Clause>

      <Clause heading="8. Reporting a vulnerability">
        <p>
          Report to {LEGAL_ENTITY.securityEmail}. We aim to acknowledge within
          two business days and to keep you updated until the issue is resolved.
          Please give us reasonable time to remediate before public disclosure;
          in return we will not pursue legal action over good-faith research that
          respects user data, avoids privacy violations and does not degrade the
          service. Do not test against another customer&rsquo;s hosts.
        </p>
      </Clause>

      <Clause heading="9. Incidents">
        <p>
          Where SigmaHub acts as processor, personal data breaches are notified
          to affected customers without undue delay and in any event within 72
          hours of becoming aware, with the detail required to meet your own
          notification duties. That commitment is contractual — see clause 8 of
          the <Link href="/dpa">DPA</Link>.
        </p>
      </Clause>
    </LegalDocument>
  );
}
