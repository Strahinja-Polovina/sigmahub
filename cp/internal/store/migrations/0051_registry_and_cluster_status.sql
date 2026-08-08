-- Two things a cluster deploy cannot work without.
--
-- 1. An image registry. Every cross-host build — a dedicated build server, and
--    EVERY cluster workload, because a scheduler decides which node runs the pod
--    — needs the built image to reach a place all the hosts can pull from. Until
--    now the build pushed a `sigmahub/<res>:<sha>` tag anonymously, which Docker
--    resolves to docker.io/sigmahub and answers with a 401. The registry is
--    per-org: it is the customer's registry, holding the customer's images.
--
-- 2. Per-node cluster status. `clusters.status` was written once, as
--    'provisioning', and nothing ever moved it: SetClusterReady had no caller, so
--    a working cluster read as provisioning forever and a broken one read the
--    same. The node rows now carry what each node reported, and the cluster's
--    status is DERIVED from them rather than set independently.

CREATE TABLE IF NOT EXISTS org_registries (
    org_id     TEXT PRIMARY KEY,
    -- Registry host, e.g. ghcr.io, registry.gitlab.com, 10.0.0.4:5000.
    host       TEXT NOT NULL,
    -- Repository namespace under the host (a GitHub org, a Docker Hub user).
    -- Optional: a self-hosted registry often has none.
    namespace  TEXT NOT NULL DEFAULT '',
    username   TEXT NOT NULL DEFAULT '',
    -- KMS-wrapped: a registry password is a credential, and a database leak
    -- alone must not yield push access to the customer's images.
    password_wrapped BYTEA,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE cluster_nodes
    -- pending: the node has been told to join and has not reported back.
    -- ready:   k3s is up on it (and, on the control plane, the API server answered).
    -- error:   the node reported a failure; node_message says what.
    ADD COLUMN IF NOT EXISTS node_status  TEXT NOT NULL DEFAULT 'pending',
    ADD COLUMN IF NOT EXISTS node_message TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS reported_at  TIMESTAMPTZ;
