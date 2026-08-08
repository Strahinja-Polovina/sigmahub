-- Dedicated build servers.
--
-- Building an image is the most resource-hungry thing that happens on a host:
-- it saturates CPU and disk for minutes at a time, on the same machine that is
-- serving traffic. A branch map can now name a server to build on, so the build
-- runs on a machine chosen for it and only the resulting image is shipped to the
-- deploy target.
ALTER TABLE git_branch_map
    ADD COLUMN IF NOT EXISTS build_server_id TEXT REFERENCES servers(id) ON DELETE SET NULL;

-- The deployment records where the build actually ran, so a deploy's history
-- explains itself after the mapping changes.
ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS build_server_id TEXT;

CREATE INDEX IF NOT EXISTS deployments_build_server_idx
    ON deployments (build_server_id) WHERE build_server_id IS NOT NULL;
