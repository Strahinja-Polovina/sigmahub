# Staging bring-up + acceptance (P2 beta launch, SIGMA-53/54)

A turnkey runbook for standing up a full-stack SigmaHub staging environment and
proving it works before design partners touch it. The whole control plane +
dashboard + Postgres come up from `docker-compose.yml`; `smoke.sh` verifies the
core path end to end.

Everything an operator must do by hand is called out — the rest is one command.

## 0. Prerequisites (operator)

- A host with Docker + Docker Compose.
- A DNS name for the dashboard (e.g. `staging.sigmahub.example`) pointing at the
  host, and one for the control plane if it's exposed separately.
- One or more **BYO SSH-reachable servers** to enroll as SigmaHub hosts (we use
  SSH onboarding only — no provider API keys).

## 1. Configure secrets

```
cd cp/deploy
cp .env.example .env
```

Fill in `.env` (all required unless noted):

| Var | What |
| --- | --- |
| `CP_DB_PASSWORD` | Postgres password (generated). |
| `CP_PROVISION_TOKEN` | Gates org provisioning — `openssl rand -hex 32`. |
| `BETTER_AUTH_SECRET` | Dashboard session key — `openssl rand -base64 32`. |
| `WEB_PUBLIC_URL` | `https://staging.sigmahub.example` (cookies/redirects). |
| `SIGMAHUB_CP_PUBLIC_URL` | Public URL a BYO host dials to reach the CP (e.g. `https://cp.staging.sigmahub.example`) — the in-cluster `http://cp:8080` is not reachable from a host. Rendered into the install command. |
| `SIGMAHUB_AGENT_VERSION` | Released agent tag the control plane installs (e.g. `v0.3.0`) — there is no asset published under `latest`. It becomes the CP's `CP_AGENT_VERSION`; the dashboard has no copy, it asks the CP. Required here because staging builds from source, which stamps no release tag. |
| `CP_RELEASE_TOKEN` | GitHub token with `contents:read` on the release repository. Required whenever that repository is **private** — the control plane proxies install.sh and the release assets with it, and a private repo answers 404 to an unauthenticated fetch, so onboarding fails at the first curl without it. Leave empty for a public release repo (the anonymous path has a higher rate limit). |
| `CP_RELEASE_REPO` | Which repository's releases those are. Leave unset unless you run your own fork — the default is the upstream slug that `install.sh`'s pinned cosign identity already expects. |
| `CP_ACME_EMAIL` | Let's Encrypt contact for managed-domain TLS. |
| `CP_ACME_CA_DIR_URL` | Optional — point at LE staging/Pebble so repeated bring-ups don't spend the real CA's issuance budget. |
| `CP_HUGGING_FACE_TOKEN` | Optional — the Hub account the picker searches as and the agent downloads weights as. Empty still serves public models; gated ones (Llama & co) stay invisible. |
| `CP_KMS_BACKEND` | `file` is fine for staging; `vault` for prod custody. |
| `CP_DB_ENGINES` / `CP_S3_ENGINES` | Leave default to exercise every engine. |
| `CP_PADDLE_*` | Optional — leave empty; billing degrades to the honest usage preview. |
| `CP_VM_WRITE_URL` / `CP_LOKI_URL` | Optional — telemetry shows not-configured until set. |

> **Key custody:** the file backend generates the KMS key on first boot inside
> the `cp-data` volume. **Back that volume up** — every wrapped secret depends on
> it. For a prod-shaped staging, set `CP_KMS_BACKEND=vault` + `CP_VAULT_*`.

## 2. Bring the stack up

```
docker compose -f cp/deploy/docker-compose.yml up -d --build
```

This starts Postgres (with the dashboard's own DB seeded via `initdb/`), the
control plane (migrations run on boot), and the dashboard. Watch it settle:

```
docker compose -f cp/deploy/docker-compose.yml logs -f cp web
```

## 3. Verify with the smoke check

From anywhere that can reach the control plane:

```
CP_URL=https://cp.staging.sigmahub.example \
CP_PROVISION_TOKEN=<CP_PROVISION_TOKEN from .env> \
cp/deploy/smoke.sh
```

`smoke.sh` asserts, against the **running** stack (no mocks):

1. `/healthz` + `/readyz` (DB reachable).
2. Org provisioning mints an Org-Admin token.
3. Unauthenticated and wrong-token requests are rejected (401).
4. Project + environment create.
5. An agent-enrollment (bootstrap) token issues — the SSH-onboarding entry point.
6. Read models reflect the writes.

Exit 0 = the control plane is healthy and the core provisioning path works. It
provisions a uniquely-named throwaway org each run.

## 4. Enroll a host (SSH onboarding)

In the dashboard (or via `POST /v1/orgs/{org}/servers/provision`), add a server;
drop the returned bootstrap public key onto the host's `authorized_keys` and run
the printed one-liner to install `sigmad`. Every URL in that one-liner is the
control plane's: it serves `install.sh` and proxies that release's assets with a
server-side GitHub credential, so a PRIVATE release repository onboards without
the host ever talking to github.com. Which release is the control plane's answer
alone (`SIGMAHUB_AGENT_VERSION` → `CP_AGENT_VERSION`) — it comes back with the
bootstrap token and the dashboard renders it, so the command and the assets
cannot name different versions. It points the agent at `SIGMAHUB_CP_PUBLIC_URL`,
which must be `https://`: the command pipes `install.sh` into `sudo bash`, and
that script is the one artifact cosign cannot cover, because it is what runs
cosign.
The agent enrolls, joins the WireGuard mesh, and appears under **Servers**.
Attach it to the `prod` environment to schedule resources on it.

## 5. Acceptance checks (SIGMA-54)

With at least one enrolled host, verify the P1-13 acceptance bar. These read
real state through the API/dashboard — no fabricated numbers:

- **Agent overhead:** the agent's own CPU/RAM stays within budget on an idle
  host — check the server's metrics panel (self-telemetry) after ~30 min.
- **Tenant isolation:** a token scoped to org A cannot read org B — cross-org
  reads 404/401 (the smoke check already demonstrates unauth rejection; extend
  it with a second org to assert cross-tenant 404).
- **Caps:** resource creation respects the per-server availability matrix (an
  incompatible kind/server pairing is refused with a typed 422).
- **Backups green streak:** enable backups + PITR on a Postgres resource; after
  the first daily cycle, `GET /v1/orgs/{org}/backups/verify-days` shows a green
  day (an unrestored backup counts as no backup). This is also the start of the
  **30-day restore-verify clock** — from here, the streak must stay unbroken.

## 6. Start the 30-day restore-verify clock

Enabling backups (step 5) begins the automated daily restore-verify. Record the
start date; the launch gate is 30 consecutive green days on the verify feed.
Nothing else to do — the CP schedules it; just don't let the streak break.

## 7. Watch the control plane's own loops (SIGMA-248)

`GET /metrics` (unauthenticated, Prometheus text format, no tenant data in any
label) reports the control plane's own health. The series that matters is:

```
sigmahub_cp_loop_last_success_seconds{loop="..."}
```

one per background loop — `reconciler_resync`, `deploy_drain`,
`backup_scheduler`, `alert_dispatcher`, `sweeper`, `usage_sweep`. It is the
unix time of that loop's last pass that did **all** of its work; `0` means it
has never completed one. A loop that is erroring on every tick keeps logging
and keeps ticking, so from outside "running" and "working" are otherwise the
same observation — and the failure that motivates this is silent by
construction: when `CreateDueBackupRuns` starts erroring, no backup run is
created for any tenant, and `backup_failed` cannot fire because it needs a run
to exist before it can fail.

Alert on staleness, generously relative to each loop's interval (the resync
runs every 60s, the usage sweep every 10 minutes):

```
- alert: SigmahubCPLoopStalled
  expr: time() - sigmahub_cp_loop_last_success_seconds > 1800
  for: 10m
```

Include `== 0` in the same alert: a loop that never started must page, and a
zero timestamp is how that looks. `sigmahub_cp_loop_errors_total` distinguishes
"spinning and failing" from "not running at all", and
`sigmahub_cp_db_pool_connections{state="acquired"}` at the ceiling is a control
plane about to stall every loop at once.

This is the CP's own health, deliberately separate from the tenant-facing
telemetry pipeline: the alert path for customer events is the per-org outbox,
which is itself one of these loops, so a control plane in trouble cannot be
relied on to report that it is in trouble. Scrape this from outside.

## 8. Retention (SIGMA-249)

The sweeper retires the append-only tables in bounded batches every 30s. The
defaults, set in `cp/cmd/sigmahub-cp/main.go` and argued there:

| table | kept | notes |
| --- | --- | --- |
| `server_metrics` | 24h | pre-existing |
| `deploy_logs` | 30d after the deployment **finished** | by far the largest table; an in-flight build is never touched |
| `cp_audit_log` | 400d | just over a year, so an annual review can look back |
| `deploy_requests` | 30d | drained rows only; queued ones are undeployed pushes |
| `webhook_deliveries` | 30d | redelivery dedup; providers retry for hours |
| `alert_outbox` | 90d | `sent`/`failed` only; a pending row is an undelivered alert |
| `idempotency_keys` | 7d | a client retry arrives in seconds |

Without this the disk is the limit: a 200-server install doing 30 deploys a day
writes ~27M `deploy_logs` rows a year, and a full disk takes Postgres
read-only, which fails every tenant at once.

## Teardown

```
docker compose -f cp/deploy/docker-compose.yml down          # keep volumes
docker compose -f cp/deploy/docker-compose.yml down -v       # wipe everything
```

`down -v` deletes the `cp-data` volume (and the KMS key) — only for a throwaway
staging you intend to rebuild from scratch.
