-- P1-8 Traefik ingress + automatic Let's Encrypt. A custom domain is attached to
-- an "app" resource; the reconciler renders Traefik router labels onto that
-- resource's container and a proxy.traefik op onto its (proxy-role) server, and
-- Traefik's ACME resolver issues the certificate. Cert status/serial/expiry are
-- reported back by the agent and stored here for the domain-management UI.
--
-- challenge_type is an abstraction point: 'http' (HTTP-01) and 'tls-alpn'
-- (TLS-ALPN-01) are usable now; 'dns' (DNS-01 wildcard) is a HOOK ONLY — the
-- provider credential lives in dns_provider_credentials but end-to-end DNS-01
-- issuance, the provider choice, and its acceptance move to P1-12 (previews).

CREATE TABLE IF NOT EXISTS domains (
    id              TEXT PRIMARY KEY,                       -- dom_<hex>
    org_id          TEXT NOT NULL,
    resource_id     TEXT NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
    domain          TEXT NOT NULL,                          -- fqdn, lowercased
    challenge_type  TEXT NOT NULL DEFAULT 'http',           -- 'http' | 'tls-alpn' | 'dns'
    -- Reported by the agent from Traefik's ACME store. cert_status advances
    -- pending -> issuing -> issued (or failed); serial makes issuance idempotency
    -- observable (a re-apply must not change it).
    cert_status     TEXT NOT NULL DEFAULT 'pending',        -- pending|issuing|issued|failed
    cert_serial     TEXT,
    cert_expires_at TIMESTAMPTZ,
    last_error      TEXT,
    created_by      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- A domain routes to exactly one resource (no ambiguous ingress).
CREATE UNIQUE INDEX IF NOT EXISTS domains_domain_idx ON domains (lower(domain));
CREATE INDEX IF NOT EXISTS domains_resource_idx ON domains (resource_id);
CREATE INDEX IF NOT EXISTS domains_org_idx ON domains (org_id);

-- DNS-01 provider credential — the reduced "genuine hook": stores a KMS-wrapped
-- provider token per org so the cert path has a credential interface to read.
-- No provider is wired in P1-8; DNS-01 issuance is P1-12.
CREATE TABLE IF NOT EXISTS dns_provider_credentials (
    id             TEXT PRIMARY KEY,                        -- dnsp_<hex>
    org_id         TEXT NOT NULL,
    provider       TEXT NOT NULL,                           -- e.g. 'cloudflare'
    token_wrapped  BYTEA NOT NULL,                          -- P1-6 envelope
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS dns_provider_org_idx ON dns_provider_credentials (org_id, provider);
