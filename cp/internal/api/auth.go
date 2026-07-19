package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

		// Actor identity (P1-1): the web app forwards the acting user in a
		// header signed with the service token plaintext (a secret both ends
		// already share). The actor can only narrow the token's role, never
		// exceed it — so per-user Developer/Project Admin gates are enforced
		// here, not per deployment.
		actorB64 := r.Header.Get("X-Sigmahub-Actor")
		// SIGMA-82: in strict mode an org-scoped token MUST carry an actor, so a
		// stolen/misused user-facing token can't act with its full (unnarrowed)
		// role. The dev wildcard token (OrgID "*") is a system bypass and exempt.
		if s.requireActor && actorB64 == "" && p.OrgID != "*" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "actor header required"})
			return
		}
		if actorB64 != "" {
			actor, err := verifyActor(actorB64, r.Header.Get("X-Sigmahub-Actor-Signature"), tok)
			if err != nil {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid actor header"})
				return
			}
			if actor.Name != "" {
				p.Name = actor.Name
			}
			if actor.Role.AtLeast(p.Role) {
				// actor >= token: token caps the effective role, keep p.Role.
			} else {
				p.Role = actor.Role
			}
		}

		if !p.Role.AtLeast(minRole) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "insufficient role"})
			return
		}
		next(w, r.WithContext(withPrincipal(r.Context(), p)))
	}
}

type actorClaim struct {
	Name string
	Role store.Role
}

// verifyActor checks the HMAC-SHA256 signature (keyed with the bearer token
// plaintext) over the raw base64 payload, then parses it.
func verifyActor(actorB64, sigHex, bearerToken string) (actorClaim, error) {
	mac := hmac.New(sha256.New, []byte(bearerToken))
	mac.Write([]byte(actorB64))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(sigHex)
	if err != nil || !hmac.Equal(want, got) {
		return actorClaim{}, errors.New("bad actor signature")
	}

	raw, err := base64.RawURLEncoding.DecodeString(actorB64)
	if err != nil {
		return actorClaim{}, err
	}
	var payload struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return actorClaim{}, err
	}
	role, err := store.ParseRole(payload.Role)
	if err != nil {
		return actorClaim{}, err
	}
	return actorClaim{Name: payload.Name, Role: role}, nil
}

// requireProvision gates the org-provisioning endpoint: a dedicated provision
// token (prod) or the dev wildcard token (dev). Constant-time compares.
func (s *Server) requireProvision(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		okProv := s.provisionToken != "" &&
			subtle.ConstantTimeCompare([]byte(tok), []byte(s.provisionToken)) == 1
		okDev := s.devServiceToken != "" &&
			subtle.ConstantTimeCompare([]byte(tok), []byte(s.devServiceToken)) == 1
		if tok == "" || (!okProv && !okDev) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "provisioning requires the provision token"})
			return
		}
		next(w, r)
	}
}
