package api

// Compose placement endpoints: read the app's service graph with its current
// placement, and move services between servers / set per-service env.

import (
	"context"
	"net/http"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// ComposeAPI is the placement slice of the store.
type ComposeAPI interface {
	ComposeServicesForResource(ctx context.Context, orgID, resourceID string) ([]store.ComposeServiceView, string, error)
	SetComposePlacements(ctx context.Context, orgID, resourceID string, placements []store.ComposePlacement, actor string) ([]string, error)
}

func (s *Server) handleGetComposeServices(w http.ResponseWriter, r *http.Request) {
	if s.compose == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "compose placement is not configured"})
		return
	}
	services, homeServer, err := s.compose.ComposeServicesForResource(r.Context(),
		r.PathValue("orgId"), r.PathValue("resourceId"))
	if err != nil {
		s.writeStoreErr(w, err, "compose services")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"services": services,
		// Services with no explicit placement run here.
		"homeServerId": homeServer,
	})
}

func (s *Server) handleSetComposePlacements(w http.ResponseWriter, r *http.Request) {
	if s.compose == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "compose placement is not configured"})
		return
	}
	var req struct {
		Placements []store.ComposePlacement `json:"placements"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	orgID, resourceID := r.PathValue("orgId"), r.PathValue("resourceId")
	affected, err := s.compose.SetComposePlacements(r.Context(), orgID, resourceID,
		req.Placements, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "set compose placements")
		return
	}

	// Moving a service changes what runs where, and a live container only picks
	// up a changed spec through a fresh rollout generation, so mint a config
	// deployment before re-rendering (SIGMA-166). Every affected server is
	// re-rendered — including the one a service just LEFT, whose document must
	// stop describing it or an orphan container keeps running there.
	s.mintConfigDeploys(r, orgID, []string{resourceID}, "compose placement changed")
	if s.reconcile != nil {
		for _, serverID := range affected {
			s.reconcile.ReconcileAsync(orgID, serverID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "servers": affected})
}
