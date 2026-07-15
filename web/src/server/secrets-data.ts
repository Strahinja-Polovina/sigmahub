import "server-only";

// Secret data access shared by the read path (resource Environment tab) and the
// write actions. Dual-mode like the rest of the app: with SIGMAHUB_CP_URL set,
// values are envelope-encrypted in the control plane and only cross this
// boundary via the audited reveal; without it, the demo path keeps a local
// table so the dashboard stays functional offline (explicitly NOT the encrypted
// production store — a dev convenience).

import { client } from "./db";
import {
  cpEnabled,
  cpListSecrets,
  cpCreateSecret,
  cpRevealSecret,
  cpDeleteSecret,
  type CpActor,
} from "./cp";

export type SecretScope = "project" | "environment";

/** Metadata for one secret — never its value. */
export type SecretMeta = {
  id: string;
  name: string;
  envVar: boolean;
  scope: SecretScope;
};

function sid() {
  return `sec_${crypto.randomUUID().replace(/-/g, "").slice(0, 12)}`;
}

let demoTableReady = false;
async function ensureDemoSecretsTable() {
  if (demoTableReady) return;
  await client.query(`CREATE TABLE IF NOT EXISTS demo_secrets (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL,
    project_id     TEXT NOT NULL,
    environment_id TEXT,
    name           TEXT NOT NULL,
    value          TEXT NOT NULL,
    env_var        BOOLEAN NOT NULL DEFAULT false,
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
  )`);
  // Mirror the CP's scoping: a name is unique within a project's project-scoped
  // secrets, and within one environment — but the two scopes can reuse a name
  // (an environment override of a project default).
  await client.query(
    `CREATE UNIQUE INDEX IF NOT EXISTS demo_secrets_project_name_idx
       ON demo_secrets (project_id, lower(name)) WHERE environment_id IS NULL`
  );
  await client.query(
    `CREATE UNIQUE INDEX IF NOT EXISTS demo_secrets_env_name_idx
       ON demo_secrets (environment_id, lower(name)) WHERE environment_id IS NOT NULL`
  );
  demoTableReady = true;
}

/** The secrets a resource inherits: its environment's own secrets plus the
 *  project-scoped ones, with the environment winning on a name clash (matching
 *  the agent-side ResolveSecretsForResource precedence). Metadata only. */
export async function effectiveSecrets(
  orgId: string,
  projectId: string,
  environmentId: string
): Promise<SecretMeta[]> {
  let rows: SecretMeta[];
  if (cpEnabled()) {
    const all = await cpListSecrets(orgId, projectId);
    rows = all
      .filter((s) => s.environmentId === null || s.environmentId === environmentId)
      .map((s) => ({
        id: s.id,
        name: s.name,
        envVar: s.envVar,
        scope: s.environmentId ? ("environment" as const) : ("project" as const),
      }));
  } else {
    await ensureDemoSecretsTable();
    const res = await client.query<{
      id: string;
      name: string;
      env_var: boolean;
      environment_id: string | null;
    }>(
      `SELECT id, name, env_var, environment_id FROM demo_secrets
        WHERE org_id = $1 AND project_id = $2
          AND (environment_id IS NULL OR environment_id = $3)
        ORDER BY name`,
      [orgId, projectId, environmentId]
    );
    rows = res.rows.map((r) => ({
      id: r.id,
      name: r.name,
      envVar: r.env_var,
      scope: r.environment_id ? ("environment" as const) : ("project" as const),
    }));
  }
  // Dedupe by name, environment scope winning over project scope.
  const byName = new Map<string, SecretMeta>();
  for (const s of rows) {
    const prev = byName.get(s.name);
    if (!prev || s.scope === "environment") byName.set(s.name, s);
  }
  return [...byName.values()].sort((a, b) => a.name.localeCompare(b.name));
}

/** Create a secret in the given scope. Caller has already authorized. */
export async function putSecret(
  orgId: string,
  projectId: string,
  input: { name: string; value: string; environmentId: string; envVar: boolean },
  actor: CpActor
): Promise<void> {
  if (cpEnabled()) {
    await cpCreateSecret(orgId, projectId, input, actor);
    return;
  }
  await ensureDemoSecretsTable();
  try {
    await client.query(
      `INSERT INTO demo_secrets (id, org_id, project_id, environment_id, name, value, env_var, created_by)
       VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
      [
        sid(),
        orgId,
        projectId,
        input.environmentId || null,
        input.name,
        input.value,
        input.envVar,
        actor.name,
      ]
    );
  } catch (err) {
    if (err instanceof Error && /unique|duplicate/i.test(err.message)) {
      throw new Error(`A secret named "${input.name}" already exists in this scope.`);
    }
    throw err;
  }
}

/** Reveal a secret's plaintext value. Caller has already authorized (Project
 *  Admin+); the CP additionally audits the read against the forwarded actor. */
export async function readSecretValue(
  orgId: string,
  secretId: string,
  actor: CpActor
): Promise<string> {
  if (cpEnabled()) {
    return cpRevealSecret(orgId, secretId, actor);
  }
  await ensureDemoSecretsTable();
  const res = await client.query<{ value: string }>(
    `SELECT value FROM demo_secrets WHERE org_id = $1 AND id = $2`,
    [orgId, secretId]
  );
  const value = res.rows[0]?.value;
  if (value === undefined) throw new Error("Secret not found.");
  return value;
}

/** Return a secret's name (for the audit target) without decrypting its value. */
export async function secretName(orgId: string, secretId: string): Promise<string | null> {
  if (cpEnabled()) {
    // The CP exposes name via list; resolving it here would need the projectId,
    // which the delete path does not carry. The CP audits the delete with the
    // real name server-side, so the web audit target can stay coarse.
    return null;
  }
  await ensureDemoSecretsTable();
  const res = await client.query<{ name: string }>(
    `SELECT name FROM demo_secrets WHERE org_id = $1 AND id = $2`,
    [orgId, secretId]
  );
  return res.rows[0]?.name ?? null;
}

export async function removeSecret(
  orgId: string,
  secretId: string,
  actor: CpActor
): Promise<void> {
  if (cpEnabled()) {
    await cpDeleteSecret(orgId, secretId, actor);
    return;
  }
  await ensureDemoSecretsTable();
  await client.query(`DELETE FROM demo_secrets WHERE org_id = $1 AND id = $2`, [orgId, secretId]);
}
