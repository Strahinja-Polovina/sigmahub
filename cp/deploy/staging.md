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
| `WEB_PUBLIC_URL` | `https://staging.sigmahub.example` (cookies/redirects, and the site address the bundled proxy serves the dashboard on). |
| `SIGMAHUB_CP_PUBLIC_URL` | Public URL a BYO host dials to reach the CP (e.g. `https://cp.staging.sigmahub.example`) — the in-cluster `http://cp:8080` is not reachable from a host. Rendered into the install command. |
| `CP_AGENT_VERSION` | Released agent tag the control plane installs (e.g. `v0.3.0`) — there is no asset published under `latest`. Spelled the way the refusal that asks for it is spelled (SIGMA-269); the dashboard has no copy, it asks the CP. Required here because staging builds from source, which stamps no release tag. |
| `CP_RELEASE_TOKEN` | GitHub token with `contents:read` on the release repository. Required whenever that repository is **private** — the control plane proxies install.sh and the release assets with it, and a private repo answers 404 to an unauthenticated fetch, so onboarding fails at the first curl without it. Leave empty for a public release repo (the anonymous path has a higher rate limit). |
| `CP_RELEASE_REPO` | Which repository's releases those are. **Leave unset.** The value is fixed to the upstream slug and the control plane refuses to boot with any other — `install.sh` bakes in the cosign identity it verifies against, so a fork's artifacts would be downloaded and then rejected on the host. Serving a fork needs a forked `install.sh` and agent build, not this setting. |
| `CP_ACME_EMAIL` | Let's Encrypt contact for managed-domain TLS. |
| `CP_ACME_CA_DIR_URL` | Optional — point at LE staging/Pebble so repeated bring-ups don't spend the real CA's issuance budget. |
| `CP_HUGGING_FACE_TOKEN` | Optional — the Hub account the model picker searches as. Empty still serves public models; gated ones (Llama & co) stay invisible to the picker. It is NOT delivered to tenants: a tenant needing gated weights supplies their own `HUGGING_FACE_HUB_TOKEN` project secret (SIGMA-302). |
| `CP_APPS_DOMAIN` | Optional — the wildcard resource URLs are minted under, e.g. `apps.example.com` with `*.apps.example.com` pointed at the proxy servers. Empty falls back to sslip.io derived from each host's public address, so a resource is reachable on its first deploy with no DNS record at all (SIGMA-351). |
| `CP_KMS_BACKEND` | `file` is fine for staging; `vault` for prod custody. |
| `CP_DB_ENGINES` / `CP_S3_ENGINES` | Leave **empty** to exercise every engine the catalog defines — the list is derived, not written down (SIGMA-268). Set one only to cut it (`CP_DB_ENGINES=postgres`). |
| `CP_PADDLE_*` | Optional — leave empty; billing degrades to the honest usage preview. Setting `CP_PADDLE_API_KEY` **and** `CP_PADDLE_PRICE_ID` also switches on the free-tier ceiling (SIGMA-363): an org using all 3 free units with no live subscription is refused *new servers* until it subscribes. Nothing running is touched, and resources on servers an org already has are unaffected. Left empty — the self-hosted case — nothing is ever capped, because a paywall on a deployment with no checkout is just a broken product. |
| `CP_VM_WRITE_URL` / `CP_LOKI_URL` | Optional — telemetry shows not-configured until set. |
| `SMTP_HOST` / `SMTP_FROM` | **Set these before opening public sign-up.** They turn on real mail delivery for invites, email verification and password resets. Optional `SMTP_PORT` (default 587; 465 uses implicit TLS), `SMTP_USERNAME`, `SMTP_PASSWORD`, and `SMTP_ALLOW_INSECURE` for a submission server on a trusted network that offers no STARTTLS (off by default — these messages carry bearer links). |
| `HEALTH_PROBE_TOKEN` | Optional shared secret for the dashboard's `GET /api/health?require=cp` deploy gate. Unset leaves the probe open. |
| `CP_ALERTMANAGER_URL` | Optional — where `COMPOSE_PROFILES=monitoring` sends the control plane's own alerts (see §7). |

> **Mail is a launch gate.** With no `SMTP_HOST`/`SMTP_FROM`, every invite,
> verification and reset link is written to the web container's log and delivered
> to nobody. That is genuinely workable self-hosted — the operator relays the
> link, and every screen says so instead of pointing users at a spam folder — but
> a locked-out user on a public sign-up cannot ask anyone for it. Setting these
> also flips email verification on by default, which is what makes invite
> acceptance safe: the address match is otherwise the only thing binding an invite
> to a person, and anyone can register claiming any address (SIGMA-361).

> **Key custody:** the file backend generates the KMS key on first boot inside
> the `cp-data` volume. **Back that volume up, independently of the database** —
> it is the single root of all recoverability. Every tenant secret, database
> credential, backup-target secret and restic repository password is sealed under
> an org key wrapped by it, including the ones protecting snapshots already
> sitting in the customer's own bucket. Losing it does not degrade the install; it
> makes the entire encrypted corpus permanently unreadable, offsite backups
> included, with no recovery path by design (that property is also what makes
> org purge a real erasure). Restoring a database backup without the matching
> custody root restores ciphertext and nothing else. For a prod-shaped staging,
> set `CP_KMS_BACKEND=vault` + `CP_VAULT_*` and back the Vault transit key up the
> way Vault documents; rotation is supported in place (`RotateKEK`) and re-wraps
> every envelope, so a rotated root never strands a live secret.

## 2. Bring the stack up

```
docker compose -f cp/deploy/docker-compose.yml up -d --build
```

This starts Postgres (with the dashboard's own DB seeded via `initdb/`), the
control plane (migrations run on boot), and the dashboard. Watch it settle:

```
docker compose -f cp/deploy/docker-compose.yml logs -f cp web
```

## 2b. TLS (SIGMA-266) — the https names above have to answer

`up -d` also starts `proxy`, a Caddy terminator on ports 80 and 443. It is the
only thing published to the world: `cp` and `web` bind to `127.0.0.1` so they
are reachable from the host for on-box checks and from nowhere else. Nothing to
configure — its two site addresses are `SIGMAHUB_CP_PUBLIC_URL` and
`WEB_PUBLIC_URL` from `.env` (see `cp/deploy/Caddyfile`), and it obtains and
renews certificates over ACME itself.

Operator prerequisites, which are not optional:

- **Both DNS names resolve to this host** before the first request. Caddy gets a
  certificate on demand, so a name that does not resolve simply fails to get
  one.
- **80 and 443 are open** to the internet. Port 80 carries the HTTP-01
  challenge and the redirect to https; blocking it blocks issuance.
- The `caddy-data` volume holds the ACME account and the issued certificates.
  `down -v` throws it away and the next bring-up re-issues, which spends the
  CA's per-domain issuance budget — the same reason `CP_ACME_CA_DIR_URL` exists
  for the control plane's own managed-domain TLS.

Check it before moving on — the second command is the one that matters, because
it is the artifact the trust model turns on:

```
curl -fsS https://staging.sigmahub.example/ -o /dev/null && echo dashboard ok
curl -fsSI https://cp.staging.sigmahub.example/install.sh | head -1
```

If `SIGMAHUB_CP_PUBLIC_URL` names an **http** URL, the control plane refuses
`/install.sh` with a message naming `CP_PUBLIC_URL`, and the connect-server
wizard shows that sentence instead of a command. That is deliberate and is not
a bug to work around by making the URL http: the install command pipes
`install.sh` into `sudo bash`, and that script is the one artifact cosign cannot
cover — it is what runs cosign — so plaintext there is root on every host being
onboarded for anyone on the path, plus a one-time bootstrap token in the clear.

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
provisions a uniquely-named throwaway org each run, and revokes every Org Admin
token it minted on the way out — including on a failed or interrupted run
(SIGMA-267). The empty orgs stay (there is no org teardown API in v1); the
authority over them does not, and no credential is left on disk.

**This is the same check CI runs.** `.github/workflows/deploy-staging.yml` runs
`smoke.sh` on the box at the end of every rollout and fails the deploy on a
non-zero exit, so the manual step above and the automated gate are one artifact
rather than two that can disagree (SIGMA-265). The rollout also polls the
dashboard's `GET /api/health?require=cp`, which round-trips to the control plane
with the dashboard's own credential — the readiness gate used to be the
marketing home page, which renders without ever touching the control plane and
therefore reported success for every way the web→CP path can break.

## 4. Enroll a host (SSH onboarding)

In the dashboard (or via `POST /v1/orgs/{org}/servers/provision`), add a server;
drop the returned bootstrap public key onto the host's `authorized_keys` and run
the printed one-liner to install `sigmad`. Every URL in that one-liner is the
control plane's: it serves `install.sh` and proxies that release's assets with a
server-side GitHub credential, so a PRIVATE release repository onboards without
the host ever talking to github.com. Which release is the control plane's answer
alone (`CP_AGENT_VERSION`, under that one name in `.env` and in the container) —
it comes back with the bootstrap token and the dashboard renders it, so the
command and the assets cannot name different versions. It points the agent at
`SIGMAHUB_CP_PUBLIC_URL`, which must be `https://`: the command pipes
`install.sh` into `sudo bash`, and that script is the one artifact cosign cannot
cover, because it is what runs cosign. The control plane refuses to serve it
over anything else (step 2b).
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

### `GET /livez` — the zero-infrastructure version

If you run no monitoring stack at all, this is the one thing to wire up:

```
curl -sf http://localhost:8080/livez   # 200 "alive", 503 once a loop is wedged
```

It applies the staleness judgement below **inside** the control plane, per loop,
with a budget sized to each loop's own interval, and names the wedged loops in
the body. Point a Kubernetes `livenessProbe`, a Docker healthcheck, an uptime
checker or a cron at it and the failure class this section is about stops being
invisible without any of the rest of this.

It is deliberately **not** `/readyz`. A wedged background loop does not stop the
API serving reads and writes, so the correct response is to restart the process,
not to pull it out of the load balancer and take the dashboard down as well.
`/readyz` stays "can I serve" (a database ping); `/livez` is "should I be
restarted".

### The alerting stack, shipped

```
COMPOSE_PROFILES=monitoring docker compose up -d
```

brings up `vmsingle` (scrapes `cp:8080/metrics` every 30s, config in
`deploy/monitoring/scrape.yml`) and `vmalert` (rules in
`deploy/monitoring/alerts.yml`). Set `CP_ALERTMANAGER_URL` to have the alerts
page someone; without it they are still visible in vmalert's UI and as the
`ALERTS` series. The rules are the ones this section used to only describe:

```
- alert: SigmahubCPLoopStalled
  expr: time() - sigmahub_cp_loop_last_success_seconds > 1800
  for: 10m
```

plus `SigmahubCPLoopNeverStarted` (`== 0` — a loop that never started must page,
and a zero timestamp is how that looks), `SigmahubCPLoopErroring`
(`sigmahub_cp_loop_errors_total`, which distinguishes "spinning and failing" from
"not running at all"), `SigmahubCPPoolSaturated` (a pool at its ceiling is a
control plane about to stall every loop at once) and
`SigmahubCPResyncOutrunningTick`.

This profile is separate from `telemetry` on purpose. That one is the **tenant**
pipeline (customer logs and metrics); this one watches the control plane itself,
has to work on a deployment that offers no tenant telemetry at all, and has to
keep working when the tenant pipeline is the thing that broke. Same reasoning one
level up: the alert path for customer events is the per-org outbox, which is
itself one of these loops, so a control plane in trouble cannot be relied on to
report that it is in trouble. Scrape it from outside where you can.

## 7b. Before you open sign-up to the internet (SIGMA-365)

Three things are configuration, not code, and all three are off in a bare
bring-up. A closed beta with trusted tenants is fine without them; a public
sign-up page is not.

1. **Mail.** `SMTP_HOST` + `SMTP_FROM` (§1). Without them invites and password
   resets reach nobody, and email verification — the thing that makes an invited
   address mean a person — stays off.
2. **Edge rate limiting.** better-auth throttles the credential endpoints inside
   the dashboard, which is the whole request path for this single-instance
   deployment. Add a CDN/WAF or a `caddy-ratelimit` build (the exact block is
   commented in `Caddyfile`) before exposing sign-in publicly, and note that the
   in-process counters stop being sufficient the moment a second `web` replica
   exists.
3. **Monitoring.** `COMPOSE_PROFILES=monitoring`, or at minimum a check against
   `GET /livez` (§7). Otherwise a wedged control plane is silent.

Billing enforcement (§1, `CP_PADDLE_*`) is the fourth switch: without it every
org grows without limit, which is correct self-hosted and wrong for a hosted
tenant.

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

## 9. Migrations and rollback (SIGMA-290)

Both halves migrate on boot: the control plane runs its embedded SQL migrations
from `setupStore`, and the dashboard runs its drizzle migrations from
`instrumentation.ts`. Each run now holds a session-level Postgres advisory lock
on `hashtext('sigmahub:migrate')`, so a second process starting mid-migration
**waits** instead of racing the DDL. Before that lock existed, two replicas
starting milliseconds apart both saw the same file unapplied, one created the
object and the other failed with `relation "x" already exists`, exited 1, and
was restarted by `unless-stopped` — a crash loop whose only clue was a
migration that had plainly already applied.

Migration is also available as an explicit **deploy step**, which is what you
want as soon as more than one replica exists:

```
docker compose -f cp/deploy/docker-compose.yml run --rm cp migrate
```

`sigmahub-cp migrate` applies the schema and exits. It needs only
`CP_DATABASE_URL` — no KMS backend, no key material.

### The rollback contract

Rollback here is "edit `CP_IMAGE` / `WEB_IMAGE` in `cp/deploy/.env` and
`compose up -d`". That runs the **N-1 binary against schema N**, because
rolling an image back does not roll the schema back. So:

- **Every migration must be additive.** New tables, new nullable columns, new
  indexes. A column the N-1 binary does not know about is invisible to it; a
  column it *needs* that you dropped is an outage it cannot be rolled back out
  of.
- **Never drop or rename a column or table in the same release that stops using
  it.** Stop writing it in release N, stop reading it in N+1, drop it in N+2 —
  by which point N-1 is no longer a rollback target you would reach for.
- **Never add a NOT NULL column without a default**, and never narrow a type or
  add a constraint an N-1 write would violate: the old binary is still writing
  rows while you decide whether to roll forward again.
- **Roll the schema forward only.** If a migration is wrong, fix it with another
  migration. There are no down-migrations, by design — a down-migration is a
  destructive operation run at the worst possible moment.

### Sizing the migration window

Each migration file runs in one transaction under the advisory lock above, so
while a long one is applying, **every other replica's boot blocks on that lock**.
Two shipped migrations do full-table work and are worth a maintenance window on a
large install rather than a rolling restart:

- `0063_server_hours_unit_weight.sql` backfills `server_hours`, the one meter
  table that grows a row per running server per hour and is therefore unbounded
  in install age.
- `0065_resource_public_label.sql` backfills `resources` and then builds a unique
  index **without** `CONCURRENTLY` (it cannot: the runner is inside a
  transaction), which holds a `SHARE` lock blocking writes to `resources` for the
  duration.

Both are correct and both are one-time. On a fresh or small install they are
imperceptible. Run `migrate` as the explicit deploy step above, watch it finish,
then roll the binaries — do not discover the lock wait as a stalled rollout.

## 10. Running more than one control-plane replica (SIGMA-291)

Nearly all of the control plane's shared state already lives in Postgres —
advisory locks around reconciles and migrations, `SKIP LOCKED` work leases,
partial unique indexes — so a second replica is safe for correctness. Two things
are worth knowing before you scale `cp` past one instance.

**Agent long-poll wake-ups are shared, and must stay that way.** The
reconciler's long-poll waiter map is per-process. A change rendered on replica B
is announced to replica A over Postgres `LISTEN`/`NOTIFY`
(`SubscribeDSDChanges` / `PublishDSDChange`, wired in `main.go`), so an agent
polling A is woken immediately. Without that fan-out roughly half of all DSD
changes would be delivered a full long-poll window (25s) late, with no error
anywhere: deploys and config changes would simply feel intermittently sluggish.
The delivery is best-effort — `NOTIFY` is not durable — which is fine, because
a missed wake costs one long-poll window and the 60s fleet resync re-renders
regardless.

**The telemetry ingest budget is per replica, not per org.**
`orgSamplesPerSec` / `orgLinesPerSec` in `cp/internal/telemetry` are in-memory
token buckets, so N replicas give each org roughly N times the intended budget.
This is a stated limitation, not a bug to be surprised by: divide those
constants by your replica count, or enforce the real limit in front of the
metrics/logs sink. It is deliberately not backed by a shared counter — that
would put a database round trip on the highest-frequency request the control
plane serves, to tighten a backstop that already sits behind the agent-side
metric allowlist and series cap.

## Teardown

```
docker compose -f cp/deploy/docker-compose.yml down          # keep volumes
docker compose -f cp/deploy/docker-compose.yml down -v       # wipe everything
```

`down -v` deletes the `cp-data` volume (and the KMS key) — only for a throwaway
staging you intend to rebuild from scratch.
