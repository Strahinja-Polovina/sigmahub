-- SIGMA-171: the billed quantity was sent to Paddle exactly once, at checkout.
-- Nothing ever pushed it again, so an org that grew from 4 connected servers to
-- 24 kept being charged for 1 while the dashboard showed the live figure as
-- "Total due". These two columns let a sweep reconcile the subscription without
-- fighting the webhook: `quantity` stays Paddle's echoed truth (ordered by
-- last_event_at), and these record what WE last pushed, purely to debounce the
-- retry when an echo is slow or lost.
ALTER TABLE org_billing
	ADD COLUMN IF NOT EXISTS synced_quantity    integer,
	ADD COLUMN IF NOT EXISTS quantity_synced_at timestamptz;
