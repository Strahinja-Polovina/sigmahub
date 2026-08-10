import type { Metadata } from "next";
import Link from "next/link";

import {
  Bullets,
  Clause,
  LEGAL_ENTITY,
  LegalDocument,
  SUB_PROCESSORS,
} from "@/components/marketing/legal";

export const metadata: Metadata = {
  title: "Data Processing Agreement — SigmaHub",
  description:
    "The Article 28 terms under which SigmaHub processes personal data on your behalf, including the sub-processor list.",
};

export default function DpaPage() {
  return (
    <LegalDocument slug="/dpa">
      <Clause heading="1. Roles and scope">
        <p>
          This Data Processing Agreement forms part of the{" "}
          <Link href="/terms">Terms of Service</Link> and applies whenever{" "}
          {LEGAL_ENTITY.tradingName} processes personal data on your behalf. You
          are the <strong>controller</strong>; {LEGAL_ENTITY.tradingName} is the{" "}
          <strong>processor</strong>. It takes effect for your organisation when
          you accept the Terms — you do not have to negotiate a separate document
          to be covered, though a countersigned copy is available on request from{" "}
          {LEGAL_ENTITY.dpoEmail}.
        </p>
        <p>
          Personal data {LEGAL_ENTITY.tradingName} processes for its own purposes
          — your account, your billing, keeping the platform secure — is
          controller processing and is covered by the{" "}
          <Link href="/privacy">Privacy Policy</Link> instead.
        </p>
      </Clause>

      <Clause heading="2. Subject matter, duration, nature and purpose">
        <Bullets
          items={[
            <>
              <strong>Subject matter:</strong> operation of the SigmaHub control
              plane over servers you own, including deployment, supervision,
              managed databases, object storage, backups, DNS and telemetry.
            </>,
            <>
              <strong>Duration:</strong> for as long as your organisation exists,
              plus the deletion window in clause 9.
            </>,
            <>
              <strong>Nature and purpose:</strong> storage, transmission,
              indexing and display of the data your workloads produce, and of the
              configuration required to run them. No profiling, no automated
              decision-making, no use of your data for model training.
            </>,
          ]}
        />
      </Clause>

      <Clause heading="3. Categories of data and data subjects">
        <p>
          You determine what enters your workloads, so the exhaustive list is
          yours to state. In practice the categories are:
        </p>
        <Bullets
          items={[
            <>
              <strong>Data subjects:</strong> your end users, your employees and
              contractors who use the dashboard, and anyone whose data your
              application happens to store or log.
            </>,
            <>
              <strong>Categories:</strong> identifiers and contact details in your
              databases and object storage; whatever appears in your
              containers&rsquo; standard output and standard error, which is
              forwarded verbatim and routinely includes IP addresses, user
              identifiers and email addresses; backup contents; and the account
              and role data of your dashboard users.
            </>,
            <>
              <strong>Special categories:</strong> not expected. If your workload
              processes special-category or criminal-offence data, tell us before
              onboarding so the arrangement can be assessed rather than assumed.
            </>,
          ]}
        />
      </Clause>

      <Clause heading="4. Processor obligations">
        <Bullets
          items={[
            <>
              Process personal data only on your documented instructions —
              your use of the product being the primary instruction — unless
              required otherwise by law, in which case we tell you first unless
              that law forbids it.
            </>,
            <>
              Bind every person authorised to process the data to confidentiality.
            </>,
            <>
              Implement the technical and organisational measures described in{" "}
              <Link href="/security">Security</Link>, which is incorporated into
              this agreement by reference.
            </>,
            <>
              Assist you, taking into account the nature of the processing, with
              data subject requests, with security obligations, with breach
              notification and with data protection impact assessments.
            </>,
          ]}
        />
      </Clause>

      <Clause heading="5. Sub-processors">
        <p>
          You give general authorisation for the sub-processors listed below. We
          will give at least 30 days&rsquo; notice before adding or replacing
          one; you may object on reasonable data-protection grounds, and if the
          objection cannot be resolved you may terminate the affected service
          without penalty. Each sub-processor is bound by terms no less
          protective than these.
        </p>
        <div className="overflow-x-auto rounded-lg border border-border">
          <table className="w-full min-w-[46rem] border-collapse text-left text-sm">
            <thead className="bg-muted/60">
              <tr>
                <th className="px-4 py-3 font-medium text-foreground">
                  Sub-processor
                </th>
                <th className="px-4 py-3 font-medium text-foreground">Purpose</th>
                <th className="px-4 py-3 font-medium text-foreground">
                  Data received
                </th>
                <th className="px-4 py-3 font-medium text-foreground">Applies</th>
              </tr>
            </thead>
            <tbody>
              {SUB_PROCESSORS.map((sp) => (
                <tr key={sp.name} className="border-t border-border align-top">
                  <td className="px-4 py-3 font-medium text-foreground">
                    {sp.name}
                  </td>
                  <td className="px-4 py-3">{sp.purpose}</td>
                  <td className="px-4 py-3">{sp.data}</td>
                  <td className="px-4 py-3">{sp.scope}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <p>
          <strong>Your own servers are not sub-processors.</strong> SigmaHub
          never sells, rents or resells infrastructure; the hosts stay on your
          contract with your provider, who is your sub-processor, not ours.
        </p>
      </Clause>

      <Clause heading="6. International transfers">
        <p>
          Where a sub-processor above processes personal data outside the EEA or
          the UK, the transfer relies on the European Commission&rsquo;s Standard
          Contractual Clauses (module two, controller to processor, or module
          three where onward), and on the UK International Data Transfer Addendum
          where UK data is involved, together with the measures in{" "}
          <Link href="/security">Security</Link>. The region in which the control
          plane itself runs is set by its operator and is disclosed on request.
        </p>
      </Clause>

      <Clause heading="7. Data subject requests">
        <p>
          If a data subject contacts {LEGAL_ENTITY.tradingName} directly about
          data we hold on your behalf, we will not respond substantively but will
          forward the request to you without undue delay. Where you cannot fulfil
          a request through the dashboard yourself, we will assist — including
          locating and deleting the log lines and metric series belonging to your
          organisation.
        </p>
      </Clause>

      <Clause heading="8. Personal data breaches">
        <p>
          We will notify you without undue delay, and in any event within 72
          hours of becoming aware of a personal data breach affecting your data,
          with the nature of the breach, the categories and approximate number of
          records concerned, the likely consequences and the measures taken. The
          notification goes to your organisation&rsquo;s owners.
        </p>
      </Clause>

      <Clause heading="9. Deletion and return, and how offboarding works">
        <p>
          On termination, or on request, we delete the personal data we process
          on your behalf. Offboarding an organisation is a single operation with
          three parts, all of which run:
        </p>
        <Bullets
          items={[
            <>
              The organisation is tombstoned so no agent, token or session
              belonging to it can be used again.
            </>,
            <>
              Its rows in the control-plane database are deleted — servers,
              resources, deployments, secrets, backups, audit log.
            </>,
            <>
              Per-tenant deletes are issued to the log store and the metrics
              store, so its log lines and metric series are removed without
              touching any other tenant&rsquo;s.
            </>,
          ]}
        />
        <p>
          Backups already written to targets <em>you</em> own are yours to
          delete; we cannot reach them. Invoices are retained for the statutory
          period as described in the{" "}
          <Link href="/privacy">Privacy Policy</Link>. An export can be requested
          before teardown.
        </p>
      </Clause>

      <Clause heading="10. Audits">
        <p>
          On reasonable notice, and no more than once a year unless a supervisory
          authority requires otherwise, we will make available the information
          needed to demonstrate compliance with Article 28 and will contribute to
          an audit conducted by you or an independent auditor bound by
          confidentiality. Note the honest position in clause 7 of{" "}
          <Link href="/security">Security</Link>: there is no SOC 2 report to
          hand you in place of that audit yet.
        </p>
      </Clause>

      <Clause heading="11. Contact">
        <p>
          Data protection enquiries, countersigned copies and sub-processor
          notifications: {LEGAL_ENTITY.dpoEmail}.
        </p>
      </Clause>
    </LegalDocument>
  );
}
