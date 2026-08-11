-- SIGMA-351: give every resource a stable label of its own, so SigmaHub can
-- hand the user a working URL before they have touched DNS.
--
-- Routing today is built solely from customer-attached custom domains:
-- traefikLabels returns nil when a resource has none, and says so in its own
-- comment. An app therefore deploys, passes its health gate, shows a green
-- Running badge — and no address on the internet reaches it. PR previews are
-- worse off: ensurePreviewTx deliberately does not copy domains, on the stated
-- assumption that previews are "reachable through the operator's wildcard
-- setup", which has never existed in this codebase.
--
-- The LABEL is stored and the SUFFIX is resolved at render time, and that split
-- is the point:
--
--   * the label must survive a rename, or a customer renaming their app silently
--     breaks every link they have shared;
--   * the suffix cannot be stored, because it depends on deployment config
--     (CP_APPS_DOMAIN) and, in the fallback, on the host's current public
--     address — both of which can change under a resource that has not.
ALTER TABLE resources ADD COLUMN IF NOT EXISTS public_label TEXT NOT NULL DEFAULT '';

-- One label per install: two resources sharing one would collide into a single
-- Traefik router and serve each other's traffic. Partial, so the pre-backfill
-- empty string is not a uniqueness constraint on itself.
CREATE UNIQUE INDEX IF NOT EXISTS resources_public_label_idx
    ON resources (public_label) WHERE public_label <> '';

-- Backfill every existing resource. The id is already unique and its suffix is
-- random hex, so a label derived from name + id suffix is unique by
-- construction and needs no retry loop here.
--
-- lower(name), non-alphanumerics collapsed to single hyphens, trimmed of leading
-- and trailing hyphens, capped so the leftmost DNS label stays inside 63 bytes
-- once the id suffix is appended. A name that reduces to nothing (all punctuation,
-- or non-Latin) falls back to the kind, so the label is always dns-shaped.
UPDATE resources
   SET public_label = left(
         COALESCE(
           NULLIF(trim(both '-' from regexp_replace(lower(name), '[^a-z0-9]+', '-', 'g')), ''),
           kind
         ), 40) || '-' || right(id, 8)
 WHERE public_label = '';
