package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// DSDStore is the slice of the store the DSD endpoints need.
type DSDStore interface {
	GetDSD(ctx context.Context, serverID string) (dsd.Signed, error)
	CurrentDSDVersion(ctx context.Context, serverID string) (int64, error)
	ApplyDSDStatus(ctx context.Context, serverID string, version int64, opStatus map[string]json.RawMessage) (bool, error)
	MarkDestructiveOpApplied(ctx context.Context, id string) error
}

// DSDWaiter lets the long-poll handler block until a server's DSD changes.
type DSDWaiter interface {
	Wait(serverID string) (<-chan struct{}, func())
}

// longPollTimeout bounds how long GET /v1/agent/dsd blocks before returning
// 204; the agent immediately re-requests, so this is just the keepalive
// cadence. Kept well under typical proxy idle timeouts. A var so integration
// tests can shorten it.
var longPollTimeout = 25 * time.Second

// SetLongPollTimeout overrides the DSD long-poll window (tests only).
func SetLongPollTimeout(d time.Duration) { longPollTimeout = d }

// handleGetDSD is the agent's outbound-only long-poll for its DSD. With
// ?after=<version>, it returns immediately when a newer signed DSD exists,
// otherwise blocks up to longPollTimeout for the next change (204 on timeout).
func (s *Server) handleGetDSD(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)

	for {
		// Subscribe BEFORE reading the version. A notify only wakes waiters
		// that are already registered, so reading first and subscribing second
		// would lose a change committed in that window (the channel is closed
		// against an empty waiter set) and stall delivery until timeout. With
		// this ordering, any change committed after Wait fires our channel, and
		// any change committed before Wait is caught by the read below.
		ch, cancel := s.dsdWaiter.Wait(srv.ID)

		cur, err := s.dsdStore.CurrentDSDVersion(r.Context(), srv.ID)
		if err != nil {
			cancel()
			s.log.Error("dsd version", "err", err, "server", srv.ID)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if cur > after {
			cancel()
			signed, err := s.dsdStore.GetDSD(r.Context(), srv.ID)
			if err != nil {
				s.log.Error("dsd fetch", "err", err, "server", srv.ID)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			writeJSON(w, http.StatusOK, signed)
			return
		}

		select {
		case <-ch:
			cancel()
			// Loop: re-subscribe, re-read version, and return the new DSD.
		case <-time.After(longPollTimeout):
			cancel()
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			cancel()
			return
		}
	}
}

// deployPhase maps a deploy-pipeline op id to its (phase, resourceID). The
// rollout op keeps the res:<id> id (so its status also routes to resources.status).
func deployPhase(opID string) (phase, resourceID string, ok bool) {
	switch {
	case strings.HasPrefix(opID, "clone:"):
		return "clone", strings.TrimPrefix(opID, "clone:"), true
	case strings.HasPrefix(opID, "build:"):
		return "build", strings.TrimPrefix(opID, "build:"), true
	case strings.HasPrefix(opID, "res:"):
		return "rollout", strings.TrimPrefix(opID, "res:"), true
	}
	return "", "", false
}

type dsdStatusRequest struct {
	Version int64 `json:"version"`
	// Ops maps op id -> reported status object; resource.sync ops carry the
	// resource id in the op id ("res:<id>"), so the CP can route status into
	// resources.status.
	Ops map[string]json.RawMessage `json:"ops"`
}

func (s *Server) handleDSDStatus(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	var req dsdStatusRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	// Route "res:<id>" op statuses into resources.status, and mark applied
	// "volrm:<pendingId>" destructive ops so they drop out of future DSDs.
	byResource := map[string]json.RawMessage{}
	for opID, st := range req.Ops {
		// Advance the in-flight deployment (P1-9) as its pipeline ops report. A
		// no-op for non-git resources (no in-flight deployment).
		var os struct {
			State string `json:"state"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(st, &os)
		if phase, resID, isDeploy := deployPhase(opID); isDeploy && os.State != "" {
			if err := s.store.AdvanceDeploymentForResource(r.Context(), srv.ID, resID, phase, os.State == "applied", os.Error); err != nil {
				s.log.Error("advance deployment", "err", err, "op", opID)
			}
		}
		switch {
		case strings.HasPrefix(opID, "res:"):
			byResource[strings.TrimPrefix(opID, "res:")] = st
		case strings.HasPrefix(opID, "volrm:"):
			var os struct {
				State string `json:"state"`
			}
			if json.Unmarshal(st, &os) == nil && os.State == "applied" {
				if err := s.dsdStore.MarkDestructiveOpApplied(r.Context(), strings.TrimPrefix(opID, "volrm:")); err != nil {
					s.log.Error("mark destructive op applied", "err", err, "op", opID)
				}
			}
		}
	}
	applied, err := s.dsdStore.ApplyDSDStatus(r.Context(), srv.ID, req.Version, byResource)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no DSD for this server"})
		return
	}
	if err != nil {
		s.log.Error("dsd status", "err", err, "server", srv.ID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": applied})
}

// dsdPublicKeyB64 is the base64 of the CP's DSD-signing public key, served in
// the register response so agents can pin it.
func (s *Server) dsdPublicKeyB64() string {
	if s.dsdPub == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(s.dsdPub)
}

var _ = ed25519.PublicKey(nil)
