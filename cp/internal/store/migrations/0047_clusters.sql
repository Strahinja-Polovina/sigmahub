-- Kubernetes clusters (k3s).
--
-- A cluster is a set of the org's OWN servers promoted into a control plane and
-- workers. It belongs to an environment, so a project deploys to "the cluster in
-- staging" the same way it deploys to a server there.
--
-- Databases deliberately cannot live in a cluster. Stateful engines in a
-- scheduler mean node affinity, PV lifecycle and eviction semantics we do not
-- model, and losing a database to a rescheduling event is not a failure mode
-- worth shipping — managed databases stay on their own server and the cluster
-- reaches them over the mesh. The rule is enforced in the store, not just the UI.
CREATE TABLE IF NOT EXISTS clusters (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL,
    environment_id TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    -- provisioning: waiting for the control plane node to report ready.
    -- ready: the API server is up and workers may join.
    -- degraded: a node stopped heartbeating.
    status         TEXT NOT NULL DEFAULT 'provisioning',
    -- k3s cluster token, KMS-wrapped: workers authenticate their join with it,
    -- so it is a credential and never leaves the CP in plaintext.
    join_token_wrapped BYTEA,
    -- API server endpoint (mesh address of the control-plane node), filled in
    -- once that node reports the cluster up.
    api_endpoint   TEXT NOT NULL DEFAULT '',
    kubernetes_version TEXT NOT NULL DEFAULT '',
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS clusters_org_idx ON clusters (org_id, created_at DESC);
CREATE INDEX IF NOT EXISTS clusters_env_idx ON clusters (environment_id);

-- One cluster per environment keeps "deploy to the cluster" unambiguous.
CREATE UNIQUE INDEX IF NOT EXISTS clusters_env_uniq ON clusters (environment_id);

CREATE TABLE IF NOT EXISTS cluster_nodes (
    cluster_id TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    server_id  TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    -- control-plane runs the API server (and schedules); worker only schedules.
    role       TEXT NOT NULL DEFAULT 'worker',
    joined_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster_id, server_id)
);

-- A server belongs to at most one cluster: two control planes fighting over the
-- same host is not a state worth representing.
CREATE UNIQUE INDEX IF NOT EXISTS cluster_nodes_server_uniq ON cluster_nodes (server_id);

-- Exactly one control-plane node per cluster in v1 (no HA embedded etcd yet):
-- a partial unique index makes a second one impossible rather than merely
-- discouraged.
CREATE UNIQUE INDEX IF NOT EXISTS cluster_nodes_one_control_plane
    ON cluster_nodes (cluster_id) WHERE role = 'control-plane';

-- A resource deploys either to a server (server_id) or to a cluster.
ALTER TABLE resources
    ADD COLUMN IF NOT EXISTS cluster_id TEXT REFERENCES clusters(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS resources_cluster_idx ON resources (cluster_id);
