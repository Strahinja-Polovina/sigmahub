package api

// The org's container image registry, and the agent-side credential release.
//
// Configuring a registry is what unlocks every deploy where the build and the
// run happen on different machines: a dedicated build server, and any cluster
// workload. Without one those deploys are refused up front rather than pushing
// to docker.io under a namespace nobody owns.

import (
	"context"
	"net/http"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// RegistryAPI is the image-registry slice of the store.
type RegistryAPI interface {
	SetImageRegistry(ctx context.Context, orgID string, in store.SetImageRegistryInput, actor string) (store.ImageRegistry, error)
	GetImageRegistry(ctx context.Context, orgID string) (store.ImageRegistry, bool, error)
	DeleteImageRegistry(ctx context.Context, orgID, actor string) error
	RegistryCredentialForServer(ctx context.Context, orgID, serverID string) (store.RegistryCredential, error)
}

func (s *Server) handleGetRegistry(w http.ResponseWriter, r *http.Request) {
	if s.registry == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "registries are not configured"})
		return
	}
	reg, ok, err := s.registry.GetImageRegistry(r.Context(), r.PathValue("orgId"))
	if err != nil {
		s.writeStoreErr(w, err, "get registry")
		return
	}
	if !ok {
		// Not an error: "no registry yet" is the starting state, and the
		// dashboard renders the setup form from it.
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true,
		"registry":   reg,
		// The prefix every cross-host image tag will carry, so the UI can show
		// exactly what a pushed image will be called.
		"repository": reg.Repository(),
	})
}

func (s *Server) handleSetRegistry(w http.ResponseWriter, r *http.Request) {
	if s.registry == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "registries are not configured"})
		return
	}
	var req struct {
		Host      string `json:"host"`
		Namespace string `json:"namespace"`
		Username  string `json:"username"`
		Password  string `json:"password"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	orgID := r.PathValue("orgId")
	reg, err := s.registry.SetImageRegistry(r.Context(), orgID, store.SetImageRegistryInput{
		Host:      req.Host,
		Namespace: req.Namespace,
		Username:  req.Username,
		Password:  req.Password,
	}, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "set registry")
		return
	}
	// Image tags are rendered from the repository prefix, so every server's
	// document changes the moment it does.
	s.reconcileOrg(r.Context(), orgID)
	writeJSON(w, http.StatusOK, map[string]any{"registry": reg, "repository": reg.Repository()})
}

func (s *Server) handleDeleteRegistry(w http.ResponseWriter, r *http.Request) {
	if s.registry == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "registries are not configured"})
		return
	}
	orgID := r.PathValue("orgId")
	if err := s.registry.DeleteImageRegistry(r.Context(), orgID, principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "delete registry")
		return
	}
	s.reconcileOrg(r.Context(), orgID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// handleAgentRegistryCredential releases the push/pull credential to an agent.
// Scope comes from the agent token, never from the request body, and the store
// audits every release.
//
// A valid agent token is necessary but NOT sufficient: the store also checks
// that this particular server has something to push or pull for the org before
// it unwraps anything (SIGMA-258), and answers ErrNotFound — the 404 below —
// when it does not. Membership in the org is not need: the credential is a
// registry PAT with push rights over every image the org publishes, so one
// compromised staging host must not be able to poison the fleet's images.
func (s *Server) handleAgentRegistryCredential(w http.ResponseWriter, r *http.Request) {
	if s.registry == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no registry configured"})
		return
	}
	srv := serverFrom(r)
	cred, err := s.registry.RegistryCredentialForServer(r.Context(), srv.OrgID, srv.ID)
	if err != nil {
		s.writeStoreErr(w, err, "registry credential")
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

// reconcileOrg re-renders every server in the org. Used by settings that change
// what EVERY document contains (the registry prefix), where nudging one server
// would leave the rest describing images that no longer match.
func (s *Server) reconcileOrg(ctx context.Context, orgID string) {
	if s.reconcile == nil {
		return
	}
	servers, err := s.store.ListServers(ctx, orgID)
	if err != nil {
		s.log.Error("reconcile org after registry change", "err", err, "org", orgID)
		return
	}
	for _, sv := range servers {
		s.reconcile.ReconcileAsync(orgID, sv.ID)
	}
}
