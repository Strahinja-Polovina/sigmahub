package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Domain is a custom domain attached to an app resource, plus the reported ACME
// certificate state.
type Domain struct {
	ID            string     `json:"id"`
	OrgID         string     `json:"orgId"`
	ResourceID    string     `json:"resourceId"`
	Domain        string     `json:"domain"`
	ChallengeType string     `json:"challengeType"` // "http" | "tls-alpn" | "dns"
	CertStatus    string     `json:"certStatus"`    // pending|issuing|issued|failed
	CertSerial    string     `json:"certSerial,omitempty"`
	CertExpiresAt *time.Time `json:"certExpiresAt,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
	CreatedBy     string     `json:"createdBy"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

// validChallengeTypes are the abstractions the cert path understands. 'dns' is a
// hook only in P1-8 (end-to-end DNS-01 is P1-12).
var validChallengeTypes = map[string]bool{"http": true, "tls-alpn": true, "dns": true}

// AttachDomain attaches a custom domain to an app resource. The resource must
// belong to the org and be an "app" (only apps front HTTP). A domain routes to at
// most one resource (unique). Audited.
func (s *Store) AttachDomain(ctx context.Context, orgID, resourceID, domain, challengeType, actor string) (Domain, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" || !strings.Contains(domain, ".") || strings.ContainsAny(domain, " /") {
		return Domain{}, ErrInvalid{Msg: "domain must be a valid fully-qualified hostname"}
	}
	if challengeType == "" {
		challengeType = "http"
	}
	if !validChallengeTypes[challengeType] {
		return Domain{}, ErrInvalid{Msg: `challengeType must be "http", "tls-alpn", or "dns"`}
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Domain{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var kind string
	err = tx.QueryRow(ctx,
		`SELECT kind FROM resources WHERE org_id = $1 AND id = $2`, orgID, resourceID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return Domain{}, ErrNotFound
	}
	if err != nil {
		return Domain{}, err
	}
	if kind != "app" {
		return Domain{}, ErrInvalid{Msg: "a custom domain can only be attached to an app resource"}
	}

	d := Domain{
		ID: newID("dom"), OrgID: orgID, ResourceID: resourceID, Domain: domain,
		ChallengeType: challengeType, CertStatus: "pending", CreatedBy: actor,
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO domains (id, org_id, resource_id, domain, challenge_type, created_by)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING created_at, updated_at`,
		d.ID, orgID, resourceID, domain, challengeType, actor).Scan(&d.CreatedAt, &d.UpdatedAt)
	if isUniqueViolation(err) {
		return Domain{}, fmt.Errorf("%w: domain %q is already attached", ErrConflict, domain)
	}
	if err != nil {
		return Domain{}, err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Domain attached ("+domain+")", resourceID); err != nil {
		return Domain{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Domain{}, err
	}
	return d, nil
}

// DetachDomain removes a domain (org-scoped). Audited.
func (s *Store) DetachDomain(ctx context.Context, orgID, domainID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var domain string
	err = tx.QueryRow(ctx,
		`DELETE FROM domains WHERE org_id = $1 AND id = $2 RETURNING domain`, orgID, domainID).Scan(&domain)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Domain detached ("+domain+")", domainID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListDomainsForResource returns an app resource's domains (org-scoped).
func (s *Store) ListDomainsForResource(ctx context.Context, orgID, resourceID string) ([]Domain, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, org_id, resource_id, domain, challenge_type, cert_status, cert_serial, cert_expires_at, last_error, created_by, created_at, updated_at
		  FROM domains WHERE org_id = $1 AND resource_id = $2 ORDER BY domain`, orgID, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDomains(rows)
}

// DomainsForServer returns every domain routed to a resource scheduled on the
// server, keyed by resource id — the reconciler renders Traefik labels from it.
func (s *Store) DomainsForServer(ctx context.Context, serverID string) (map[string][]Domain, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT d.id, d.org_id, d.resource_id, d.domain, d.challenge_type, d.cert_status, d.cert_serial, d.cert_expires_at, d.last_error, d.created_by, d.created_at, d.updated_at
		  FROM domains d
		  JOIN resources r ON r.id = d.resource_id
		 WHERE r.server_id = $1
		 ORDER BY d.resource_id, d.domain`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanDomains(rows)
	if err != nil {
		return nil, err
	}
	out := map[string][]Domain{}
	for _, d := range list {
		out[d.ResourceID] = append(out[d.ResourceID], d)
	}
	return out, nil
}

// SetDomainCertStatus records the ACME certificate state reported by the agent.
// Idempotent: re-reporting the same issued serial is a no-op change. status is
// one of pending|issuing|issued|failed.
func (s *Store) SetDomainCertStatus(ctx context.Context, domain, status, serial string, expiresAt *time.Time, certErr string) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	tag, err := s.Pool.Exec(ctx, `
		UPDATE domains
		   SET cert_status = $2,
		       cert_serial = COALESCE(NULLIF($3, ''), cert_serial),
		       cert_expires_at = COALESCE($4, cert_expires_at),
		       last_error = $5,
		       updated_at = now()
		 WHERE lower(domain) = $1`, domain, status, serial, expiresAt, certErr)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanDomains(rows pgx.Rows) ([]Domain, error) {
	out := []Domain{}
	for rows.Next() {
		var d Domain
		var serial, lastErr *string
		if err := rows.Scan(&d.ID, &d.OrgID, &d.ResourceID, &d.Domain, &d.ChallengeType,
			&d.CertStatus, &serial, &d.CertExpiresAt, &lastErr, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		if serial != nil {
			d.CertSerial = *serial
		}
		if lastErr != nil {
			d.LastError = *lastErr
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
