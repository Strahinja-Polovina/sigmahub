package store

// Per-service placement and environment for a Compose app.
//
// A Compose app is a graph of services, and there is no reason every service has
// to share one host: a database wants a database server, a worker wants CPU, the
// web tier wants the proxy edge. Placement lives in the resource spec (the same
// document the reconciler renders from) so there is exactly one source of truth,
// and the reconciler gates a service on any dependency placed elsewhere.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// syncServicePlacementsTx rewrites resource_service_placements for one resource
// from its STORED spec. Call it in the same transaction as any write to
// resources.spec, after the write — it reads the row back, so it cannot drift
// from what was actually persisted.
//
// The table is a derived projection, not a second source of truth: the spec
// still is (SIGMA-332). It exists because the DSD render has to ask "which
// resources put a service on this server" as a predicate an index can drive,
// and a jsonb_array_elements EXISTS over the joined resources table is not one —
// it silently took deployments_server_target_idx out of service for every
// render. The projection is a plain SQL rewrite of the same jsonb the old
// predicate walked, so the two agree by construction, including the odd case of
// a service that declares a placement but no name.
//
// Rewrite, not merge: a placement REMOVED from the spec must disappear here, or
// a server keeps rendering a service that no longer belongs to it.
func syncServicePlacementsTx(ctx context.Context, tx pgx.Tx, resourceID string) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM resource_service_placements WHERE resource_id = $1`, resourceID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO resource_service_placements (resource_id, service, server_id)
		SELECT r.id, COALESCE(svc->>'name', ''), svc->>'serverId'
		  FROM resources r
		  CROSS JOIN LATERAL jsonb_array_elements(
		       CASE WHEN jsonb_typeof(r.spec->'compose'->'services') = 'array'
		            THEN r.spec->'compose'->'services' ELSE '[]'::jsonb END) svc
		 WHERE r.id = $1 AND COALESCE(svc->>'serverId', '') <> ''
		ON CONFLICT DO NOTHING`, resourceID)
	return err
}

// ComposePlacement is one service's placement + environment override.
type ComposePlacement struct {
	Service  string            `json:"service"`
	ServerID string            `json:"serverId"`
	Env      map[string]string `json:"env,omitempty"`
}

// ComposeServiceView is what the dashboard renders: the declared service plus
// where it runs and what it depends on.
//
// It is ALSO the round-trip shape SetComposePlacements re-marshals the compose
// block through, so every field gitdetect writes into the spec has to appear
// here. A missing one is not a display gap — it is deleted from the stored spec
// the first time anyone drags a service to another server. `dockerfile` was
// exactly that: written at create, consumed by the build op, and silently
// dropped by the first placement save, after which the service rebuilt from the
// context's default Dockerfile.
type ComposeServiceView struct {
	Name       string `json:"name"`
	Build      string `json:"build,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
	Image      string `json:"image,omitempty"`
	Ports      []int  `json:"ports,omitempty"`
	// PublishedPorts and NamedVolumes are the EVIDENCE for Rollout: they are why
	// a service is recreate rather than blue-green. The reconciler needs only the
	// verdict, but the dashboard has to be able to say which exclusive resource
	// forced it, or "recreate" reads as an arbitrary product decision.
	PublishedPorts []int             `json:"publishedPorts,omitempty"`
	NamedVolumes   []string          `json:"namedVolumes,omitempty"`
	Rollout        string            `json:"rollout,omitempty"`
	DependsOn      []string          `json:"dependsOn,omitempty"`
	ServerID       string            `json:"serverId,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
}

// composeSpecShape is the slice of a resource spec this file rewrites. Only the
// compose block is touched; everything else round-trips untouched so an unknown
// field added elsewhere is never dropped by a placement edit.
type composeSpecShape struct {
	Compose *struct {
		Services []ComposeServiceView `json:"services"`
	} `json:"compose"`
}

// ComposeServicesForResource returns the app's declared services with their
// current placement, or ErrNotFound when the resource isn't a Compose app.
func (s *Store) ComposeServicesForResource(ctx context.Context, orgID, resourceID string) ([]ComposeServiceView, string, error) {
	var raw []byte
	var homeServer string
	err := s.Pool.QueryRow(ctx, `
		SELECT spec, COALESCE(server_id, '') FROM resources
		 WHERE org_id = $1 AND id = $2`, orgID, resourceID).Scan(&raw, &homeServer)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	var shape composeSpecShape
	if err := json.Unmarshal(raw, &shape); err != nil || shape.Compose == nil {
		// An app with no compose graph is not a missing resource — it is a
		// resource whose answer to "what are your compose services" is "none".
		// Returning ErrNotFound here made the two indistinguishable, and the
		// dashboard loads this graph for EVERY app: a plain Dockerfile app, which
		// is most of them, therefore rendered "Some of this page couldn't be
		// loaded — the control plane didn't answer for the service graph" on a
		// control plane that had answered immediately and correctly.
		//
		// A genuinely missing resource still 404s: that is the ErrNoRows branch
		// above, and it is the only thing that should.
		return nil, homeServer, nil
	}
	return shape.Compose.Services, homeServer, nil
}

// SetComposePlacements rewrites the placement and env of the named services.
// Services not mentioned keep whatever they had, so a partial edit is safe.
//
// Returns every server the app now touches (including the ones it just left),
// so the caller can re-render each affected document — dropping the OLD server
// would leave an orphan container running there with nothing describing it.
func (s *Store) SetComposePlacements(ctx context.Context, orgID, resourceID string, placements []ComposePlacement, actor string) (affected []string, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var raw []byte
	var homeServer string
	err = tx.QueryRow(ctx, `
		SELECT spec, COALESCE(server_id, '') FROM resources
		 WHERE org_id = $1 AND id = $2 FOR UPDATE`, orgID, resourceID).Scan(&raw, &homeServer)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	// Decode into a generic map so unrelated spec fields survive the rewrite.
	var full map[string]json.RawMessage
	if err := json.Unmarshal(raw, &full); err != nil {
		return nil, ErrInvalid{Msg: "resource spec is not an object"}
	}
	composeRaw, hasCompose := full["compose"]
	if !hasCompose {
		return nil, ErrInvalid{Msg: "this resource is not a Compose app"}
	}
	var compose struct {
		Services []ComposeServiceView `json:"services"`
	}
	if err := json.Unmarshal(composeRaw, &compose); err != nil {
		return nil, ErrInvalid{Msg: "compose spec is malformed"}
	}

	byName := map[string]int{}
	for i, svc := range compose.Services {
		byName[svc.Name] = i
	}
	// Every server the app touches BEFORE the edit — the ones it may leave.
	seen := map[string]bool{}
	for _, svc := range compose.Services {
		if svc.ServerID != "" {
			seen[svc.ServerID] = true
		}
	}
	if homeServer != "" {
		seen[homeServer] = true
	}

	for _, p := range placements {
		idx, ok := byName[strings.TrimSpace(p.Service)]
		if !ok {
			return nil, ErrInvalid{Msg: "unknown service: " + p.Service}
		}
		serverID := strings.TrimSpace(p.ServerID)
		if serverID != "" {
			// The server must belong to this org — otherwise a placement could
			// plant a container on another tenant's host.
			var owned bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM servers WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL)`,
				orgID, serverID).Scan(&owned); err != nil {
				return nil, err
			}
			if !owned {
				return nil, ErrNotFound
			}
			seen[serverID] = true
		}
		compose.Services[idx].ServerID = serverID
		compose.Services[idx].Env = p.Env
	}

	updated, err := json.Marshal(compose)
	if err != nil {
		return nil, err
	}
	full["compose"] = updated
	newSpec, err := json.Marshal(full)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE resources SET spec = $3, updated_at = now() WHERE org_id = $1 AND id = $2`,
		orgID, resourceID, newSpec); err != nil {
		return nil, err
	}
	if err := syncServicePlacementsTx(ctx, tx, resourceID); err != nil {
		return nil, err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Compose placement updated", resourceID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	affected = make([]string, 0, len(seen))
	for id := range seen {
		affected = append(affected, id)
	}
	return affected, nil
}
