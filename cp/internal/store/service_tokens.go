package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Role mirrors the v1 web membership roles.
type Role string

const (
	RoleDeveloper    Role = "Developer"
	RoleProjectAdmin Role = "Project Admin"
	RoleOrgAdmin     Role = "Org Admin"
)

var roleRank = map[Role]int{
	RoleDeveloper:    1,
	RoleProjectAdmin: 2,
	RoleOrgAdmin:     3,
}

// ParseRole accepts the canonical v1 names plus kebab/lower spellings
// ("org-admin", "developer") for CLI ergonomics.
func ParseRole(s string) (Role, error) {
	switch strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(s, "-", " "), "_", " ")) {
	case "developer", "dev":
		return RoleDeveloper, nil
	case "project admin":
		return RoleProjectAdmin, nil
	case "org admin", "admin":
		return RoleOrgAdmin, nil
	}
	return "", fmt.Errorf("unknown role %q (want %q, %q or %q)", s, RoleOrgAdmin, RoleProjectAdmin, RoleDeveloper)
}

// AtLeast reports whether r grants everything min does. Unknown roles rank 0,
// so a corrupted row can never pass a check.
func (r Role) AtLeast(min Role) bool {
	return roleRank[r] >= roleRank[min]
}

// ServicePrincipal is an authenticated dashboard/backend caller.
type ServicePrincipal struct {
	ID    string
	OrgID string // "*" only for the dev static token
	Name  string
	Role  Role
}

// IssueServiceToken mints an org-scoped service token and audits the issuance.
// The plaintext is returned once and never stored.
func (s *Store) IssueServiceToken(ctx context.Context, orgID, name string, role Role, createdBy string) (string, ServicePrincipal, error) {
	if _, ok := roleRank[role]; !ok {
		return "", ServicePrincipal{}, fmt.Errorf("invalid role %q", role)
	}
	tok, digest := s.newToken("sst")
	p := ServicePrincipal{ID: newID("st"), OrgID: orgID, Name: name, Role: role}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", ServicePrincipal{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO service_tokens (id, org_id, name, role, token_hash, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		p.ID, orgID, name, string(role), digest, createdBy); err != nil {
		return "", ServicePrincipal{}, fmt.Errorf("insert service token: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO cp_audit_log (org_id, actor, action, target)
		VALUES ($1, $2, 'Service token issued', $3)`,
		orgID, createdBy, fmt.Sprintf("%s (%s)", name, role)); err != nil {
		return "", ServicePrincipal{}, fmt.Errorf("audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", ServicePrincipal{}, err
	}
	return tok, p, nil
}

// AuthenticateServiceToken resolves a bearer credential to its principal and
// stamps last_used_at. Unknown and revoked tokens are indistinguishable.
func (s *Store) AuthenticateServiceToken(ctx context.Context, token string) (ServicePrincipal, error) {
	var p ServicePrincipal
	var role string
	err := s.Pool.QueryRow(ctx, `
		UPDATE service_tokens
		   SET last_used_at = now()
		 WHERE token_hash = $1 AND revoked_at IS NULL
		 RETURNING id, org_id, name, role`,
		s.hashToken(token),
	).Scan(&p.ID, &p.OrgID, &p.Name, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServicePrincipal{}, ErrNotFound
	}
	if err != nil {
		return ServicePrincipal{}, err
	}
	p.Role = Role(role)
	return p, nil
}
