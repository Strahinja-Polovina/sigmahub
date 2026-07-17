# GitHub App integration (SIGMA-55)

Connecting a private repository normally requires pasting a personal access
token. With a GitHub App registered, SigmaHub instead authenticates with
short-lived **installation access tokens** (~1 hour) minted from the App's
private key — nothing long-lived to paste, rotate, or leak. The PAT path
stays available as a fallback.

## How it works

- The App's RSA private key is imported once at boot and held
  **KMS-custody-wrapped** in `cp_secrets` (same boundary as the DSD signing
  key). The PEM file can be deleted after the first boot.
- The CP signs a short-lived RS256 App JWT and exchanges it at
  `POST /app/installations/{id}/access_tokens` for an installation token,
  cached per installation and refreshed with a 5-minute safety margin
  (`cp/internal/githubapp/app_auth.go`).
- Consumption points:
  - **Repo detection** (`POST …/git/detect`, connect gate): requests that
    reference an installation are read with a minted token.
  - **Clone credentials** (`DeploymentCloneCredential`): connections with an
    `installationId` deploy with a minted token; a minting failure falls back
    to the stored PAT when one exists. The agent needs no changes — its git
    credential helper already authenticates as `x-access-token`.
- The dashboard shows **Install the GitHub App** (new connections) and
  **Link GitHub App** (existing PAT connections). GitHub's post-install
  redirect lands on `<dashboard>/dashboard/github/setup`, which routes the
  `installation_id` to the right project/connection via the signed-through
  `state` parameter.

## Registering the App (one-time, org-level)

1. GitHub → your org → **Settings → Developer settings → GitHub Apps →
   New GitHub App**:
   - **App name / slug**: e.g. `sigmahub-<yourorg>` — the slug goes into
     `CP_GITHUB_APP_SLUG`.
   - **Homepage URL**: your dashboard URL.
   - **Setup URL**: `https://<dashboard-host>/dashboard/github/setup`, and
     check **Redirect on update**.
   - **Webhook URL**: `https://<cp-host>/v1/webhooks/github`;
     **Webhook secret**: the value of `CP_GITHUB_WEBHOOK_SECRET`.
   - **Repository permissions**: `Contents: Read-only`,
     `Metadata: Read-only` (automatic).
   - **Subscribe to events**: `Push`, `Pull request`.
   - Installable by **Any account** or just your org, as you prefer.
2. After creation: note the **App ID** (`CP_GITHUB_APP_ID`) and **generate a
   private key** — GitHub downloads a `*.private-key.pem`.
3. Configure the CP and restart it:

   ```
   CP_GITHUB_APP_ID=123456
   CP_GITHUB_APP_SLUG=sigmahub-yourorg
   CP_GITHUB_APP_PRIVATE_KEY_FILE=/data/github-app.pem   # first boot only
   ```

   The boot log prints `github app configured`. The key now lives
   custody-wrapped in the database; you can delete the PEM and unset
   `CP_GITHUB_APP_PRIVATE_KEY_FILE`. To rotate the key, point the variable at
   a new PEM once — a changed key re-imports.

Half-configuration (an id without a key, or a key without an id) fails boot
loudly instead of silently minting nothing.

## Connecting a repo without a token

In the project's Git panel choose **Connect repo → Install the GitHub App**,
pick the repositories on GitHub, and you land back in the connect dialog with
the installation attached — Detect and Connect work with no token. Existing
PAT connections show **Link GitHub App**; after linking, deploys switch to
installation tokens automatically (the stored PAT remains only as a fallback
when minting fails).

Webhooks are unchanged: the App's webhook posts to the same
`/v1/webhooks/github` receiver, verified against `CP_GITHUB_WEBHOOK_SECRET`.
Preview environments (P1-12) keep their same-repo-PR-only guard; installation
tokens satisfy the head-SHA fetch previews rely on for private repos.
