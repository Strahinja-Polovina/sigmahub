package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

const principalCtxKey ctxKey = 1

func withPrincipal(ctx context.Context, p store.ServicePrincipal) context.Context {
	return context.WithValue(ctx, principalCtxKey, p)
}

func principalFrom(r *http.Request) store.ServicePrincipal {
	p, _ := r.Context().Value(principalCtxKey).(store.ServicePrincipal)
	return p
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// requireService authenticates `Authorization: Bearer sst_…` against the
// service-token registry, then authorizes: the token's org must match the
// {orgId} path segment and its role must be at least minRole. The static dev
// token (dev only, never configured in prod) short-circuits as a wildcard
// Org Admin so `make dev` keeps working without minting.
func (s *Server) requireService(minRole store.Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "service token required"})
			return
		}

		var p store.ServicePrincipal
		if s.devServiceToken != "" &&
			subtle.ConstantTimeCompare([]byte(tok), []byte(s.devServiceToken)) == 1 {
			p = store.ServicePrincipal{ID: "dev", OrgID: "*", Name: "dev-service-token", Role: store.RoleOrgAdmin}
		} else {
			var err error
			p, err = s.store.AuthenticateServiceToken(r.Context(), tok)
			if errors.Is(err, store.ErrNotFound) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid service token"})
				return
			}
			if err != nil {
				s.log.Error("service auth", "err", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
		}

		if orgID := r.PathValue("orgId"); p.OrgID != "*" && p.OrgID != orgID {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "token not valid for this organization"})
			return
		}
		if !p.Role.AtLeast(minRole) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "insufficient role"})
			return
		}
		next(w, r.WithContext(withPrincipal(r.Context(), p)))
	}
}
