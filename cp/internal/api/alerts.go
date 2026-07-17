package api

// Alerting endpoints (P2-6). Channel CRUD and rules are Org Admin — channels
// receive information about the whole org, so wiring them is an org-level
// decision; the list is member-visible (metadata only, never secrets).

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// AlertSender test-fires a channel synchronously (wired from cp/internal/alerts).
type AlertSender interface {
	Send(ctx context.Context, ch store.AlertChannelSend, event, title, body string) error
}

func (s *Server) handleCreateAlertChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind   string          `json:"kind"`
		Name   string          `json:"name"`
		Config json.RawMessage `json:"config"`
		Secret string          `json:"secret"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	ch, err := s.domain.CreateAlertChannel(r.Context(), r.PathValue("orgId"), principalFrom(r).Name,
		store.CreateAlertChannelInput{Kind: req.Kind, Name: req.Name, Config: req.Config, Secret: req.Secret})
	if err != nil {
		s.writeStoreErr(w, err, "create alert channel")
		return
	}
	writeJSON(w, http.StatusCreated, ch)
}

func (s *Server) handleListAlertChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := s.domain.ListAlertChannels(r.Context(), r.PathValue("orgId"))
	if err != nil {
		s.writeStoreErr(w, err, "list alert channels")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": channels, "events": store.AlertEvents})
}

func (s *Server) handleDeleteAlertChannel(w http.ResponseWriter, r *http.Request) {
	err := s.domain.DeleteAlertChannel(r.Context(), r.PathValue("orgId"), r.PathValue("channelId"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "delete alert channel")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetAlertRules(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Events []string `json:"events"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	err := s.domain.SetAlertRules(r.Context(), r.PathValue("orgId"), r.PathValue("channelId"), req.Events, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "set alert rules")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "events": req.Events})
}

// handleTestAlertChannel fires a synchronous test notification so the UI can
// show delivery working (or the real transport error) immediately.
func (s *Server) handleTestAlertChannel(w http.ResponseWriter, r *http.Request) {
	if s.alertSender == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "alert delivery is not configured"})
		return
	}
	orgID, channelID := r.PathValue("orgId"), r.PathValue("channelId")
	ch, err := s.domain.AlertChannelForSend(r.Context(), orgID, channelID)
	if err != nil {
		s.writeStoreErr(w, err, "test alert channel")
		return
	}
	if err := s.alertSender.Send(r.Context(), ch, "test",
		"SigmaHub test notification",
		"This is a test from your SigmaHub alert settings. If you can read this, the channel works."); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}
