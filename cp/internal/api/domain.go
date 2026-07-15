package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// orgIDPattern bounds provisionable org ids so "*" (the dev wildcard) and
// other unexpected shapes can't be minted into a persisted token.
var orgIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// DomainAPI is the slice of the store the P1-1 domain endpoints need; faked
// in tests.
type DomainAPI interface {
	CreateProject(ctx context.Context, orgID, name, description, actor string) (store.Project, error)
	ListProjects(ctx context.Context, orgID string) ([]store.Project, error)
	GetProject(ctx context.Context, orgID, projectID string) (store.Project, error)
	UpdateProject(ctx context.Context, orgID, projectID, name, description, actor string) (store.Project, error)
	DeleteProject(ctx context.Context, orgID, projectID, actor string) error
	CreateEnvironment(ctx context.Context, orgID, projectID, name string, production bool, actor string) (store.Environment, error)
	ListEnvironments(ctx context.Context, orgID, projectID string) ([]store.Environment, error)
	DeleteEnvironment(ctx context.Context, orgID, envID, actor string) error
	AttachServer(ctx context.Context, orgID, envID, serverID, actor string) error
	DetachServer(ctx context.Context, orgID, envID, serverID, actor string) error
	EnvServerIDs(ctx context.Context, orgID, envID string) ([]string, error)
	CreateResource(ctx context.Context, orgID string, in store.CreateResourceInput, actor string) (store.Resource, error)
	ListResources(ctx context.Context, orgID, envID string) ([]store.Resource, error)
	DeleteResource(ctx context.Context, orgID, resourceID, actor string) error
	SetProxyRole(ctx context.Context, orgID, serverID string, proxy bool, actor string) error
	ListAudit(ctx context.Context, orgID string, limit int) ([]store.AuditEntry, error)
	IdempotencyLookup(ctx context.Context, orgID, key string) (store.IdempotentResponse, error)
	IdempotencySave(ctx context.Context, orgID, key string, in store.IdempotentResponse) (store.IdempotentResponse, error)
	IssueServiceToken(ctx context.Context, orgID, name string, role store.Role, createdBy string) (string, store.ServicePrincipal, error)
}

// writeStoreErr maps store errors onto the HTTP surface: unknown ids are 404,
// name collisions 409, domain-rule violations (availability matrix,
// unattached server) 422.
func (s *Server) writeStoreErr(w http.ResponseWriter, err error, op string) {
	var inv store.ErrInvalid
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.Is(err, store.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.As(err, &inv):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": inv.Msg})
	default:
		s.log.Error(op, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

// ── Projects ────────────────────────────────────────────────────────────────

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeJSON(w, r, &req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	p, err := s.domain.CreateProject(r.Context(), r.PathValue("orgId"),
		strings.TrimSpace(req.Name), strings.TrimSpace(req.Description), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "create project")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.domain.ListProjects(r.Context(), r.PathValue("orgId"))
	if err != nil {
		s.writeStoreErr(w, err, "list projects")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.domain.GetProject(r.Context(), r.PathValue("orgId"), r.PathValue("projectId"))
	if err != nil {
		s.writeStoreErr(w, err, "get project")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeJSON(w, r, &req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	p, err := s.domain.UpdateProject(r.Context(), r.PathValue("orgId"), r.PathValue("projectId"),
		strings.TrimSpace(req.Name), strings.TrimSpace(req.Description), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "update project")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	err := s.domain.DeleteProject(r.Context(), r.PathValue("orgId"), r.PathValue("projectId"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "delete project")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ── Environments ────────────────────────────────────────────────────────────

func (s *Server) handleCreateEnvironment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		Production bool   `json:"production"`
	}
	if err := decodeJSON(w, r, &req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	e, err := s.domain.CreateEnvironment(r.Context(), r.PathValue("orgId"), r.PathValue("projectId"),
		strings.TrimSpace(req.Name), req.Production, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "create environment")
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (s *Server) handleListEnvironments(w http.ResponseWriter, r *http.Request) {
	envs, err := s.domain.ListEnvironments(r.Context(), r.PathValue("orgId"), r.PathValue("projectId"))
	if err != nil {
		s.writeStoreErr(w, err, "list environments")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"environments": envs})
}

func (s *Server) handleDeleteEnvironment(w http.ResponseWriter, r *http.Request) {
	err := s.domain.DeleteEnvironment(r.Context(), r.PathValue("orgId"), r.PathValue("envId"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "delete environment")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ── Env ↔ server attachment ─────────────────────────────────────────────────

func (s *Server) handleAttachServer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ServerID string `json:"serverId"`
	}
	if err := decodeJSON(w, r, &req); err != nil || req.ServerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "serverId is required"})
		return
	}
	err := s.domain.AttachServer(r.Context(), r.PathValue("orgId"), r.PathValue("envId"), req.ServerID, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "attach server")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "attached"})
}

func (s *Server) handleDetachServer(w http.ResponseWriter, r *http.Request) {
	err := s.domain.DetachServer(r.Context(), r.PathValue("orgId"), r.PathValue("envId"), r.PathValue("serverId"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "detach server")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "detached"})
}

func (s *Server) handleEnvServers(w http.ResponseWriter, r *http.Request) {
	ids, err := s.domain.EnvServerIDs(r.Context(), r.PathValue("orgId"), r.PathValue("envId"))
	if err != nil {
		s.writeStoreErr(w, err, "env servers")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"serverIds": ids})
}

// ── Resources ───────────────────────────────────────────────────────────────

func (s *Server) handleCreateResource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EnvironmentID string          `json:"environmentId"`
		ServerID      string          `json:"serverId"`
		Name          string          `json:"name"`
		Kind          string          `json:"kind"`
		Spec          json.RawMessage `json:"spec"`
	}
	if err := decodeJSON(w, r, &req); err != nil ||
		strings.TrimSpace(req.Name) == "" || req.Kind == "" || req.EnvironmentID == "" || req.ServerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "environmentId, serverId, name and kind are required"})
		return
	}
	res, err := s.domain.CreateResource(r.Context(), r.PathValue("orgId"), store.CreateResourceInput{
		EnvironmentID: req.EnvironmentID,
		ServerID:      req.ServerID,
		Name:          strings.TrimSpace(req.Name),
		Kind:          req.Kind,
		Spec:          req.Spec,
	}, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "create resource")
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) handleListResources(w http.ResponseWriter, r *http.Request) {
	resources, err := s.domain.ListResources(r.Context(), r.PathValue("orgId"), r.URL.Query().Get("environmentId"))
	if err != nil {
		s.writeStoreErr(w, err, "list resources")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resources": resources})
}

func (s *Server) handleDeleteResource(w http.ResponseWriter, r *http.Request) {
	err := s.domain.DeleteResource(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "delete resource")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ── Server attributes + audit ───────────────────────────────────────────────

func (s *Server) handleProxyRole(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Proxy bool `json:"proxy"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	err := s.domain.SetProxyRole(r.Context(), r.PathValue("orgId"), r.PathValue("serverId"), req.Proxy, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "proxy role")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "proxy": req.Proxy})
}

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.domain.ListAudit(r.Context(), r.PathValue("orgId"), limit)
	if err != nil {
		s.writeStoreErr(w, err, "list audit")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// ── Org provisioning ────────────────────────────────────────────────────────

// handleProvisionOrg mints the org-scoped Org Admin service token the web app
// uses for all subsequent calls on behalf of that org — replacing the single
// wildcard dev token as the web→CP credential. Gated by the provision token
// (or the dev wildcard in dev); the plaintext is returned exactly once.
func (s *Server) handleProvisionOrg(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrgID string `json:"orgId"`
		Name  string `json:"name"`
	}
	if err := decodeJSON(w, r, &req); err != nil || req.OrgID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "orgId is required"})
		return
	}
	// Reject the wildcard and any out-of-shape id before it becomes a stored,
	// cross-tenant Org Admin token.
	if !orgIDPattern.MatchString(req.OrgID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid orgId"})
		return
	}
	label := req.Name
	if label == "" {
		label = "web:" + req.OrgID
	}
	tok, p, err := s.domain.IssueServiceToken(r.Context(), req.OrgID, label, store.RoleOrgAdmin, "provisioner")
	if err != nil {
		s.log.Error("provision org", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"orgId":     req.OrgID,
		"token":     tok,
		"tokenId":   p.ID,
		"role":      string(p.Role),
		"issuedAt":  time.Now().UTC(),
	})
}
