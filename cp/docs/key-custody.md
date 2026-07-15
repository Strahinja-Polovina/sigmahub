# Key custody hardening (P0-9)

_SIGMA-39 · Phase 0 foundations. Status: design + dev skeleton landed; production custody deferred to the hardening milestone._

This document is the threat model and design for how the SigmaHub control
plane (CP) holds secret key material, and how that design is realized in code
today versus in production. A Serbian summary follows at the end.

## Scope

The CP holds two classes of long-lived secret:

1. **The token pepper** — a 32-byte HMAC key that keys every token hash at rest
   (bootstrap, agent and service tokens). Landed in this ticket.
2. **Tenant data keys** (future) — per-org keys wrapping customer secrets
   (env vars, connection strings) once the CP stores those. Out of scope for
   P0 beyond leaving the interface able to express it.

Neither may sit in the clear in the primary database. Both are reached only
through the `KeyCustody` boundary (`cp/internal/kms`), whose every unwrap is
audited.

## Threat model

| # | Threat | Mitigation |
|---|--------|-----------|
| T1 | Database snapshot leaks (backup, replica, SQL injection read) | Tokens are stored only as HMAC-SHA256 digests keyed by the pepper; the pepper itself is stored **wrapped**, never in the clear. A DB-only leak yields neither usable tokens nor the pepper. |
| T2 | Offline brute force of leaked token hashes | The HMAC key (pepper) is not in the DB, so leaked `token_hash` rows can't be attacked without also compromising the KMS key material. |
| T3 | Operator over-reach — an operator with DB access silently reads secrets | Secret access goes through `Unwrap`, and **every unwrap emits an audit event**. In production this trail is anchored outside the primary infra (see below), so an operator can't both read a secret and erase the evidence. |
| T4 | Bulk decrypt / exfiltration of tenant data keys (future) | Interface is shaped for a production custody that requires **quorum approval + break-glass** on bulk unwrap; the dev stub does not enforce this (documented gap). |
| T5 | Key/ciphertext tampering | Wrapping is AES-256-GCM (authenticated); a flipped ciphertext bit fails `Open`. Covered by `TestUnwrapRejectsTampered`. |

Explicit non-goals for P0-9: at-rest encryption of the whole database,
network-layer secret transport (already TLS), and HSM integration.

## Design

### The `KeyCustody` boundary

```
Wrap(ctx, purpose, plaintext)   -> ciphertext      // encrypt for storage
Unwrap(ctx, purpose, ciphertext)-> plaintext       // decrypt + emit audit
AuditEvents(ctx)                -> []AuditEvent     // unwraps seen
```

The invariant that makes this worth a boundary: **`Unwrap` has an audit side
effect that callers cannot skip.** `purpose` is a non-secret label (e.g.
`token_pepper`) recorded on each unwrap.

### Token pepper lifecycle

1. On boot the CP builds a `KeyCustody`, then calls `LoadTokenPepper`.
2. First boot: generate 32 random bytes → `Wrap` → store in `cp_secrets`
   (`ON CONFLICT DO NOTHING`, so concurrent first-boots converge on one
   pepper). Later boots: read the wrapped row.
3. `Unwrap` the row → the pepper is held in memory (`Store.pepper`) and never
   written back in the clear. This unwrap writes one `cp_audit_log` row
   (`actor='kms', action='Key unwrapped', target='token_pepper'`).
4. Token hashing becomes `HMAC-SHA256(pepper, token)`. Pre-pepper rows /
   deployments fall back to plain SHA-256, so the change is not a hard cutover.

### Identity separation (T3)

The unwrap audit records the KMS action under a **system** actor, distinct
from the human operators and the `dashboard`/`sigmad` actors that appear
elsewhere in `cp_audit_log`. In production the operator identity that runs the
CP process and the unwrap-authorizing identity are separate principals; the
dev stub records the action but does not enforce the split.

## Implementation status

**Landed (this ticket):**

- `cp/internal/kms`: the `KeyCustody` interface + `FileCustody`, a dev-grade
  AES-256-GCM custody whose master key is a 0600 file
  (`CP_KMS_KEY_FILE`, default `./.data/cp-kms.key`).
- `cp_secrets` table + `LoadTokenPepper`; token hashing rekeyed to HMAC.
- Unwrap → `cp_audit_log` via `Store.AuditUnwrapSink`.
- Tests: wrap/unwrap round-trip, every-unwrap-emits-audit, key stability
  across reloads, tamper rejection. End-to-end verified: pepper stored
  wrapped, tokens authenticate, plain SHA-256 of a token is absent from the
  DB, pepper stable across CP restarts.

**Deferred to the hardening milestone (documented gaps):**

- `FileCustody` is **dev only** — the master key sits on the same host as the
  ciphertext, so it provides envelope hygiene and the audit trail, not a real
  trust boundary. Production replaces it with an external KMS/HSM
  implementation of the same interface (e.g. Vault transit / cloud KMS).
- **Unwrap audit anchored outside the primary infra** — today the audit row is
  in the same Postgres as the ciphertext. Production ships these events to an
  append-only external sink.
- **Quorum + break-glass** on bulk decrypt (T4) — not enforced by the dev stub.
- **Operator ↔ unwrap identity separation** (T3) — recorded, not enforced.

---

## Sažetak (SR)

Kontrolna ravan (CP) ne čuva tajne ključeve u čistom obliku. „Token pepper"
(HMAC ključ kojim se hešuju svi tokeni) čuva se **kriptovan** u tabeli
`cp_secrets`; otključava se preko `KeyCustody` sloja (`cp/internal/kms`) pri
podizanju servisa, a **svako otključavanje upisuje audit zapis** u
`cp_audit_log` (`actor='kms'`). Hešovanje tokena je sada `HMAC-SHA256(pepper,
token)`, pa curenje same baze ne daje upotrebljive tokene niti pepper.

U ovom tiketu je isporučen razvojni sloj: `FileCustody` (AES-256-GCM sa
master ključem u 0600 fajlu), tabela `cp_secrets`, prelazak na HMAC i audit
otključavanja — sa testovima i end-to-end proverom (pepper stabilan preko
restarta, tokeni i dalje važe, običan SHA-256 tokena ne postoji u bazi).
Za produkciju je odloženo (svesni nedostaci): eksterni KMS/HSM umesto fajl
ključa, audit otključavanja sidren van primarne infrastrukture, kvorum +
break-glass na masovno dešifrovanje, i razdvajanje identiteta operatera od
identiteta koji odobrava otključavanje.
