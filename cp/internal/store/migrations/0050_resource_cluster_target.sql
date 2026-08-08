-- A resource targets EITHER a server or a cluster.
--
-- 0047 added resources.cluster_id but left server_id NOT NULL, so the model it
-- introduced was never representable: creating a cluster workload wrote an
-- empty string into a NOT NULL column with a foreign key to servers, and every
-- cluster deploy failed on resources_server_id_fkey. The column has to be
-- nullable for a scheduled workload, which has no server of its own.
ALTER TABLE resources ALTER COLUMN server_id DROP NOT NULL;

-- Any empty strings written before the column was nullable are not real server
-- references; normalize them so the check below can hold. (The FK made a real
-- one impossible, so this only ever touches rows from a failed write path.)
UPDATE resources SET server_id = NULL WHERE server_id = '';

-- Exactly one target. Without this a resource could end up with both (which
-- placement would silently disambiguate) or neither (which nothing renders),
-- and the store's own guard would be the only thing preventing it.
ALTER TABLE resources DROP CONSTRAINT IF EXISTS resources_one_target;
ALTER TABLE resources ADD CONSTRAINT resources_one_target CHECK (
    (server_id IS NOT NULL AND cluster_id IS NULL)
 OR (server_id IS NULL AND cluster_id IS NOT NULL)
);
