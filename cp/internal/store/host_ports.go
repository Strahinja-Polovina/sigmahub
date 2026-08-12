package store

// SIGMA-352 (explicit host ports): resolve an app's requested host-port bindings
// against everything already bound on the same server, so a collision becomes a
// different port and a note — never a container that fails to bind after the
// build.
//
// Two host-port spaces share a server's network namespace:
//
//   - the mesh ports the managed engines (database, object storage, inference)
//     get from allocateMeshPort, recorded in the three port-owning tables;
//   - the explicit ports a single-container app publishes via spec.ports[].host,
//     which the wizard collects (SIGMA-210) and the reconciler renders verbatim.
//
// Nothing reconciled the two. Compose apps never had the problem — their host
// ports are deliberately dropped ("ingress is the proxy's job", see the web's
// ignoredHostPorts) — but a single-container app published exactly what the
// user typed, so two apps asking for 5432, or one asking for a port a database
// already holds, produced a container that started and immediately failed to
// bind, long after the build that caused it.
//
// The resolver keeps the requested port when it is free and moves it to the next
// free port when it is not, recording the original as `requestedHost` so the
// dashboard can say "requested 5432, running on 5433". The reconciler reads only
// `host`, so it renders the resolved port with no change of its own.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// hostPortMapping mirrors the spec.ports[] element the wizard writes and the
// reconciler reads, plus RequestedHost, which the resolver sets only when it had
// to move a binding. omitempty keeps an unmoved port's JSON identical to before.
type hostPortMapping struct {
	Container     int    `json:"container,omitempty"`
	Host          int    `json:"host,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	RequestedHost int    `json:"requestedHost,omitempty"`
}

// hostPortCeiling is the top of the range the resolver will move a colliding
// port into. Host ports are user-chosen and usually low; a service that cannot
// find a free port below this on one host has bigger problems than this resolver
// can solve.
const hostPortCeiling = 65535

// resolveAppHostPortsTx rewrites the app spec's explicit host-port bindings to
// values free on the target server, returning the spec unchanged when it
// declares none. Runs inside CreateResource's transaction under the SAME
// per-server advisory lock as allocateMeshPort, so a concurrent mesh allocation
// or a second app create cannot handed out a port this one is about to take.
//
// excludeResourceID is the resource being created — excluded from the scan so a
// re-resolve (were one ever added) does not treat its own ports as taken.
func resolveAppHostPortsTx(ctx context.Context, tx pgx.Tx, serverID, excludeResourceID string, spec json.RawMessage) (json.RawMessage, error) {
	// Preserve every other spec field: unmarshal the envelope, touch only ports.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(spec, &obj); err != nil {
		// Not an object we can reason about — leave it exactly as given rather
		// than risk dropping fields. A malformed spec is the caller's problem,
		// not this resolver's to silently rewrite.
		return spec, nil
	}
	rawPorts, ok := obj["ports"]
	if !ok {
		return spec, nil
	}
	var ports []hostPortMapping
	if err := json.Unmarshal(rawPorts, &ports); err != nil || len(ports) == 0 {
		return spec, nil
	}
	// Nothing to do unless at least one mapping publishes a host port.
	anyHost := false
	for _, p := range ports {
		if p.Host > 0 {
			anyHost = true
			break
		}
	}
	if !anyHost {
		return spec, nil
	}

	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('dbport:' || $1))`, serverID); err != nil {
		return nil, fmt.Errorf("host port lock: %w", err)
	}

	used, err := boundHostPortsTx(ctx, tx, serverID, excludeResourceID)
	if err != nil {
		return nil, err
	}

	changed := false
	for i := range ports {
		req := ports[i].Host
		if req <= 0 {
			continue
		}
		port := req
		for used[port] {
			port++
			if port > hostPortCeiling {
				return nil, fmt.Errorf("%w: %s (no free host port at or above %d)",
					ErrNoFreePort, serverID, req)
			}
		}
		used[port] = true // hold it against the other mappings in this same spec
		if port != req {
			ports[i].Host = port
			ports[i].RequestedHost = req
			changed = true
		}
	}
	if !changed {
		return spec, nil
	}

	newPorts, err := json.Marshal(ports)
	if err != nil {
		return nil, fmt.Errorf("marshal resolved ports: %w", err)
	}
	obj["ports"] = newPorts
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal resolved spec: %w", err)
	}
	return out, nil
}

// boundHostPortsTx is every host port already spoken for on a server: the mesh
// ports in the three port-owning tables, plus the explicit host ports every
// other app on that server publishes in its spec. excludeResourceID drops the
// resource being resolved from the app scan.
func boundHostPortsTx(ctx context.Context, tx pgx.Tx, serverID, excludeResourceID string) (map[int]bool, error) {
	used := map[int]bool{}

	// Mesh ports. One query over the union of the three tables that
	// allocateMeshPort also reads, so the two allocators agree on what is taken.
	rows, err := tx.Query(ctx, `
		SELECT port FROM db_credentials WHERE server_id = $1
		UNION
		SELECT port FROM s3_credentials WHERE server_id = $1
		UNION
		SELECT port FROM llm_endpoints  WHERE server_id = $1`, serverID)
	if err != nil {
		return nil, fmt.Errorf("scan mesh ports: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		used[p] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Explicit host ports from every OTHER app on this server. jsonb path: the
	// spec's compose services are exposed-only (their host ports are dropped
	// web-side), so only the single-container ports[] array binds a host port.
	appRows, err := tx.Query(ctx, `
		SELECT spec->'ports' FROM resources
		 WHERE server_id = $1 AND kind = 'app' AND id <> $2
		   AND jsonb_typeof(spec->'ports') = 'array'`, serverID, excludeResourceID)
	if err != nil {
		return nil, fmt.Errorf("scan app ports: %w", err)
	}
	defer appRows.Close()
	for appRows.Next() {
		var raw []byte
		if err := appRows.Scan(&raw); err != nil {
			return nil, err
		}
		var ports []hostPortMapping
		if err := json.Unmarshal(raw, &ports); err != nil {
			continue // a spec we cannot parse cannot reserve a port
		}
		for _, p := range ports {
			if p.Host > 0 {
				used[p.Host] = true
			}
		}
	}
	return used, appRows.Err()
}
