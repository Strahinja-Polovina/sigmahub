-- What a push actually did.
--
-- A deploy request was marked 'drained' whether it produced deployments or
-- nothing at all. A push into an environment with no app resources — the normal
-- state right after connecting a repo, before the first resource exists —
-- therefore looked identical to a successful one: the webhook was accepted, the
-- request was drained, and no deploy ever ran. The one question the user asks
-- ("I pushed, why is nothing happening?") had no answer anywhere in the product.
--
-- deployments_created records how many the drain produced, and detail carries
-- the reason when that is zero. The status vocabulary gains 'no_targets' so the
-- distinction is visible without reading a count.
ALTER TABLE deploy_requests
    ADD COLUMN IF NOT EXISTS deployments_created INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS detail TEXT NOT NULL DEFAULT '';
