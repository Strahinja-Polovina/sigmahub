-- SIGMA-202/203, demo side: the mirror learns the three things the connect
-- flow now depends on.
--
-- facts is the agent's host description. The row already had cpu and mem_gb,
-- which is everything the old detail page could show; arch, distro, disk size
-- and GPU inventory had nowhere to live, so the page could not answer "what IS
-- this machine" even in CP mode, where the control plane knew.
--
-- incompatible_reasons is why a server's status is 'incompatible', as data
-- rather than prose the dashboard reinvents — the same shape the control plane
-- returns, so both modes render one code path.
--
-- name_auto marks a name the product chose. The connect form no longer asks for
-- one; check-in fills it from the reported hostname while this is set, and an
-- operator rename clears it for good.
ALTER TABLE "servers" ADD COLUMN "facts" jsonb DEFAULT '{}'::jsonb NOT NULL;--> statement-breakpoint
ALTER TABLE "servers" ADD COLUMN "incompatible_reasons" jsonb DEFAULT '[]'::jsonb NOT NULL;--> statement-breakpoint
ALTER TABLE "servers" ADD COLUMN "name_auto" boolean DEFAULT false NOT NULL;
