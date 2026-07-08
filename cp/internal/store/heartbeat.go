package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// RecordHeartbeat updates a server's liveness from an authenticated agent
// check-in. First heartbeat flips provisioning → running; a later sweeper
// (P0-4) will handle running → unreachable on missed heartbeats.
func (s *Store) RecordHeartbeat(ctx context.Context, serverID, agentVersion string, facts json.RawMessage) error {
	facts = normalizeFacts(facts)
	tag, err := s.Pool.Exec(ctx, `
		UPDATE servers
		   SET last_seen_at = now(),
		       agent_version = COALESCE(NULLIF($2, ''), agent_version),
		       facts = $3,
		       status = CASE WHEN status IN ('provisioning', 'unreachable') THEN 'running' ELSE status END
		 WHERE id = $1`,
		serverID, agentVersion, facts)
	if err != nil {
		return fmt.Errorf("record heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
