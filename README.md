# SigmaHub

Self-hostable PaaS on servers you own: Git push-to-deploy with zero-downtime
rollouts, managed databases, encrypted backups with automated restore-verify,
and a WireGuard mesh between your servers — driven from one dashboard.

## Components

| Directory | What it is |
|---|---|
| `cp/` | **Control plane** (Go + PostgreSQL 16). Domain model, signed Desired-State Documents, deploy pipeline, secrets envelope encryption, backup scheduler, telemetry gateway. |
| `agent/` | **`sigmad` host agent** (single static Go binary). Outbound-only; applies signed DSDs: containers, WireGuard mesh, host hardening, builds, blue-green rollouts, restic backups, telemetry shipping. |
| `web/` | **Dashboard + marketing site** (Next.js). Runs in demo mode out of the box; set `SIGMAHUB_CP_URL` to drive a real control plane. |

## Quick start (development)

```sh
# 1. Control plane + its Postgres
cd cp && make db-up && make dev            # listens on :8080 (dev tokens)

# 2. Dashboard against the CP
cd web && pnpm install
SIGMAHUB_CP_URL=http://localhost:8080 pnpm dev

# 3. A server agent (on a Linux host with Docker)
cd agent && go run ./cmd/sigmad -endpoint http://localhost:8080 \
  -bootstrap-token <mint one in the dashboard>
```

## Self-hosting the control plane

- **Containers:** `cp/deploy/docker-compose.yml` (CP + Postgres; copy
  `.env.example` → `.env` first). The image builds from `cp/Dockerfile`.
- **Bare binary:** goreleaser archives ship `sigmahub-cp`; install
  `cp/packaging/sigmahub-cp.service` and configure `/etc/sigmahub-cp/env`.
- **Back up the KMS key file** (`CP_KMS_KEY_FILE`): every wrapped secret —
  tenant DEKs, the DSD signing key, token pepper — depends on it. Key custody
  design: `cp/docs/key-custody.md`.

Server onboarding uses the release installer (`agent/packaging/install.sh`),
which cosign-verifies the `sigmad` binary against the release identity before
executing anything.

## Tests

```sh
cd cp && go test ./...     # unit; integration needs CP_TEST_DATABASE_URL
cd agent && go test ./...  # unit; real-Docker e2e behind SIGMAD_DOCKER_E2E=1
cd web && pnpm test        # vitest
```

CI (`.github/workflows/ci.yml`) runs all three, the CP integration suite
against a real Postgres, and the agent Docker e2e as root. Releases
(`.github/workflows/release.yml`) are cosign-signed with per-archive SBOMs.
