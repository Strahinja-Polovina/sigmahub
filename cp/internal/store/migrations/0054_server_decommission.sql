-- Graceful decommission (SIGMA-204).
--
-- Disconnecting a server used to be a row delete: the tombstone was written,
-- the agent token revoked, and everything the platform had installed on the
-- machine — the sigmad binary, its systemd unit, the WireGuard interface and
-- config, every managed container, network and volume — stayed exactly where it
-- was. The dashboard even told the operator "the agent tears down its WireGuard
-- tunnel", which was never true. What actually happened is that the agent's
-- next heartbeat 401'd and it exited, leaving a host that looks connected to
-- anyone logged into it and is invisible to us forever.
--
-- So disconnect becomes two phases, and the row has to carry the state between
-- them:
--
--   decommission_started_at  when the CP asked. Non-null is the render trigger
--                            for the agent.uninstall op, and the clock the
--                            timeout is measured against. Deliberately NOT
--                            derived from status: the status column is written
--                            by the heartbeat path (the SIGMA-203 compatibility
--                            gate re-evaluates it on every check-in), and an
--                            in-flight teardown must not stop being rendered
--                            because a GPU host's card was pulled while it was
--                            being removed.
--   decommission_purge_volumes  whether the agent also destroys named volumes.
--                            Default FALSE, and the dialog defaults it off too:
--                            a database's data directory is the USER'S data,
--                            and "disconnect this machine" is not consent to
--                            delete it.
--   decommission_actor       who asked, so the completing transaction — which
--                            runs later, triggered by the AGENT's ack or by the
--                            sweeper's timeout, with no human in the request —
--                            can still attribute the audit row to the person
--                            who pressed the button instead of to 'sigmad'.
--
-- servers.status gains a fifth value, 'decommissioning'. Like 'incompatible'
-- (0053) it gets no CHECK constraint — the column has never had one — and like
-- it, it is deliberately not a flavour of an existing state: 'running' would
-- keep billing the machine and keep scheduling onto it, and 'unreachable' would
-- fire the sweeper's alert for a host that is doing exactly what it was told.
-- Every billing read (SweepServerHours, ConnectedServerUnits, the quantity-sync
-- high-water mark) keys on status = 'running', so a decommissioning server
-- stops being billed the moment the operator asks for it back, which is the
-- honest answer: they have told us to stop using the machine.
ALTER TABLE servers
    ADD COLUMN IF NOT EXISTS decommission_started_at    TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS decommission_purge_volumes BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS decommission_actor         TEXT    NOT NULL DEFAULT '';

-- The sweeper's timeout scan reads exactly this: live rows still in flight.
-- Partial so it stays a handful of entries on a fleet of any size.
CREATE INDEX IF NOT EXISTS servers_decommissioning_idx
    ON servers (decommission_started_at)
 WHERE decommission_started_at IS NOT NULL AND deleted_at IS NULL;
