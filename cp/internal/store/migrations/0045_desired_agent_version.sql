-- Dashboard-driven agent upgrades (agent.update): the operator picks a
-- released version; the reconciler renders an agent.update op until the
-- agent's reported version converges on it. Empty = no update requested.
ALTER TABLE servers ADD COLUMN IF NOT EXISTS desired_agent_version TEXT NOT NULL DEFAULT '';
