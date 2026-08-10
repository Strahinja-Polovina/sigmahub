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
	DeleteProject(ctx context.Context, orgID, projectID, actor string) ([]string, error)
	CreateEnvironment(ctx context.Context, orgID, projectID, name string, production bool, actor string) (store.Environment, error)
	ListEnvironments(ctx context.Context, orgID, projectID string) ([]store.Environment, error)
	UpdateEnvironmentProduction(ctx context.Context, orgID, envID string, production bool, actor string) (store.Environment, error)
	DeleteEnvironment(ctx context.Context, orgID, envID, actor string) ([]string, error)
	AttachServer(ctx context.Context, orgID, envID, serverID, actor string) error
	DetachServer(ctx context.Context, orgID, envID, serverID, actor string) error
	EnvServerIDs(ctx context.Context, orgID, envID string) ([]string, error)
	CreateResource(ctx context.Context, orgID string, in store.CreateResourceInput, actor string) (store.Resource, error)
	// ControlPlaneServerForCluster names the node a cluster workload renders
	// into. A cluster resource has no server_id, so it is the only handle a
	// mutation has on the document that has to be rebuilt.
	ControlPlaneServerForCluster(ctx context.Context, orgID, clusterID string) (string, error)
	ListResources(ctx context.Context, orgID, envID string) ([]store.Resource, error)
	DeleteResource(ctx context.Context, orgID, resourceID, actor string) (serverID string, err error)
	// ForceReapplyResource backs the unconditional Redeploy for resources with
	// no git deployment to replay (db/s3/registry apps).
	ForceReapplyResource(ctx context.Context, orgID, resourceID, actor string) (serverID string, err error)
	SetProxyRole(ctx context.Context, orgID, serverID string, proxy bool, actor string) error
	// SetServerType / RenameServer are the two things the connect form stopped
	// asking for (SIGMA-202) plus the exit from an incompatible enrollment
	// (SIGMA-203) — a type re-filing that re-runs the gate on stored facts.
	SetServerType(ctx context.Context, orgID, serverID, serverType, actor string) error
	RenameServer(ctx context.Context, orgID, serverID, name, actor string) error
	SetHardeningConfig(ctx context.Context, orgID, serverID string, keepPublicSSH, cisEnabled bool, extraPorts []store.PortException, actor string) error
	ListAudit(ctx context.Context, orgID string, limit int) ([]store.AuditEntry, error)
	IdempotencyLookup(ctx context.Context, orgID, key string) (store.IdempotentResponse, error)
	IdempotencyClaim(ctx context.Context, orgID, key string, reqHash []byte) (bool, store.IdempotentResponse, error)
	IdempotencyFinalize(ctx context.Context, orgID, key string, statusCode int, response []byte) error
	IdempotencyRelease(ctx context.Context, orgID, key string) error
	IssueServiceToken(ctx context.Context, orgID, name string, role store.Role, createdBy string) (string, store.ServicePrincipal, error)
	IssueConfirmToken(ctx context.Context, orgID, serverID, opKind, target, createdBy string, ttl time.Duration) (string, time.Time, error)
	ConfirmDestructiveOp(ctx context.Context, orgID, token, serverID, opKind, target, actor string) (string, error)
	// BeginDecommission is the graceful disconnect (SIGMA-204); DeleteServer is
	// the force path it falls back to.
	BeginDecommission(ctx context.Context, orgID, serverID string, purgeVolumes bool, actor string) (store.DecommissionState, error)
	DeleteServer(ctx context.Context, orgID, serverID, actor string) error
	RevokeAgentToken(ctx context.Context, orgID, serverID, actor string) error
	ListServiceTokens(ctx context.Context, orgID string) ([]store.ServiceTokenInfo, error)
	RevokeServiceToken(ctx context.Context, orgID, tokenID, actor string) error
	RotateServiceToken(ctx context.Context, orgID, tokenID, actor string) (string, store.ServicePrincipal, error)
	CreateSecret(ctx context.Context, orgID, actor string, in store.CreateSecretInput) (store.Secret, error)
	ListSecrets(ctx context.Context, orgID, projectID, envID string) ([]store.Secret, error)
	RevealSecret(ctx context.Context, orgID, secretID, actor string) (string, error)
	// UpdateSecretValue is the in-place rotation path (SIGMA-264): re-seal the
	// value, keep the id and every ref, mint ONE config deployment instead of
	// the two a delete-then-create costs.
	UpdateSecretValue(ctx context.Context, orgID, secretID, value, actor string) (store.Secret, error)
	DeleteSecret(ctx context.Context, orgID, secretID, actor string) (store.Secret, error)
	// Config deployments (SIGMA-166): a secret or domain change alters the
	// rendered container spec, and the standing SUCCESS target would re-render
	// it under the same rollout generation — which the agent's never-cut guard
	// refuses. Minting a 'config' deployment gives the change its own
	// generation and re-ships the running release's pinned image.
	AppResourcesForSecretScope(ctx context.Context, orgID, projectID, envID string) ([]string, error)
	CreateConfigDeployments(ctx context.Context, orgID string, resourceIDs []string, actor, reason string) ([]store.ServerRef, error)
	RotateKEK(ctx context.Context, orgID, actor string) (int, error)
	RotateDEK(ctx context.Context, orgID, actor string) (string, error)
	ReencryptSecrets(ctx context.Context, orgID string) (int, error)
	// Database resources (P1-10). Info is the Developer-visible metadata;
	// Reveal decrypts credentials (Project Admin+, audited in the store).
	GetDatabaseInfo(ctx context.Context, orgID, resourceID string) (store.DatabaseInfo, error)
	RevealDatabaseConnection(ctx context.Context, orgID, resourceID, actor string) (store.DatabaseConnection, error)
	// S3 storage (P2-1): same split — Info is member-visible, Reveal is
	// Project Admin+ and audited in the store.
	GetS3Info(ctx context.Context, orgID, resourceID string) (store.S3Info, error)
	RevealS3Connection(ctx context.Context, orgID, resourceID, actor string) (store.S3Connection, error)
	// S3 bucket/key/quota CRUD (SIGMA-65): List is member-visible; the mutations
	// return the host server id so the handler can re-render its DSD. Create key
	// returns only the access key id (the secret rides the audited agent path).
	ListBuckets(ctx context.Context, orgID, resourceID string) ([]store.Bucket, error)
	CreateBucket(ctx context.Context, orgID, resourceID, name, actor string) (store.Bucket, string, error)
	DeleteBucket(ctx context.Context, orgID, resourceID, name, actor string) (string, error)
	SetBucketQuota(ctx context.Context, orgID, resourceID, name string, quotaBytes int64, actor string) (string, error)
	CreateBucketKey(ctx context.Context, orgID, resourceID, name, actor string) (accessKey, serverID string, err error)
	// Backups (P1-11): S3-compatible targets, per-resource policy, run history,
	// the per-day verify feed and the fire-drill restore.
	CreateBackupTarget(ctx context.Context, orgID, actor string, in store.CreateBackupTargetInput) (store.BackupTarget, error)
	ListBackupTargets(ctx context.Context, orgID string) ([]store.BackupTarget, error)
	DeleteBackupTarget(ctx context.Context, orgID, targetID, actor string) error
	UpdateBackupPolicy(ctx context.Context, orgID, resourceID, actor string, in store.UpdateBackupPolicyInput) (store.BackupPolicy, error)
	ListBackupRuns(ctx context.Context, orgID, resourceID string, limit int) ([]store.BackupRun, error)
	VerifyDays(ctx context.Context, orgID string, days int) ([]store.VerifyDay, error)
	CreateRestoreRun(ctx context.Context, orgID, sourceResourceID, newResourceID, actor string) (store.BackupRun, error)
	CreateRestoreToTimestampRun(ctx context.Context, orgID, sourceResourceID, newResourceID string, targetTime time.Time, actor string) (store.BackupRun, error)
	// Repo-key custody (SIGMA-170): the retained keys of deleted resources, and
	// the audited plaintext export an operator needs to open snapshots the
	// control plane can no longer reach.
	ListArchivedRepoKeys(ctx context.Context, orgID string) ([]store.ArchivedRepoKey, error)
	ExportRepoKey(ctx context.Context, orgID, resourceID, actor string) (string, error)
	// Custom domains (P1-8). Attach/Detach return the host server id so the
	// handler can re-render its DSD.
	AttachDomain(ctx context.Context, orgID, resourceID, domain, challengeType, actor string) (store.Domain, string, error)
	DetachDomain(ctx context.Context, orgID, domainID, actor string) (serverID, resourceID string, err error)
	ListDomainsForResource(ctx context.Context, orgID, resourceID string) ([]store.Domain, error)
	// Deployments (P1-9). List is the release history; RollbackTargets are the
	// retained-image candidates; CreateRollback queues a rebuild-free rollback and
	// returns the server to re-render; GetDeployment + DeployLogsSince back the
	// build-log stream.
	ListDeployments(ctx context.Context, orgID, resourceID string, limit int) ([]store.Deployment, error)
	ListOrgDeployments(ctx context.Context, orgID string, recentLimit int) (store.OrgDeployments, error)
	RollbackTargets(ctx context.Context, orgID, resourceID string, limit int) ([]store.Deployment, error)
	CreateRollback(ctx context.Context, orgID, resourceID, targetDeploymentID, actor string) (store.Deployment, string, error)
	CreateManualRedeploy(ctx context.Context, orgID, resourceID, actor string) (store.Deployment, string, error)
	GetDeployment(ctx context.Context, orgID, deploymentID string) (store.Deployment, error)
	DeployLogsSince(ctx context.Context, deploymentID string, afterID int64, limit int) ([]store.DeployLog, error)
	// Alerting (P2-6): channels + per-event rules; ForSend resolves a
	// channel's transport config (incl. the unwrapped secret) for test-fires.
	CreateAlertChannel(ctx context.Context, orgID, actor string, in store.CreateAlertChannelInput) (store.AlertChannel, error)
	ListAlertChannels(ctx context.Context, orgID string) ([]store.AlertChannel, error)
	DeleteAlertChannel(ctx context.Context, orgID, channelID, actor string) error
	SetAlertRules(ctx context.Context, orgID, channelID string, events []string, actor string) error
	AlertChannelForSend(ctx context.Context, orgID, channelID string) (store.AlertChannelSend, error)
}

// writeStoreErr maps store errors onto the HTTP surface: unknown ids are 404,
// name collisions 409, domain-rule violations (availability matrix,
// unattached server) 422.
func (s *Server) writeStoreErr(w http.ResponseWriter, err error, op string) {
	var inv store.ErrInvalid
	var notClusterable store.ErrKindNotClusterable
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.Is(err, store.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	// A stateful kind aimed at a cluster is a domain-rule refusal like any
	// other, but it is its own error type rather than an ErrInvalid, so it fell
	// through to the 500 branch: the one refusal whose whole point is explaining
	// itself reached the client as "internal error".
	case errors.As(err, &notClusterable):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": notClusterable.Error()})
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
	orgID := r.PathValue("orgId")
	servers, err := s.domain.DeleteProject(r.Context(), orgID, r.PathValue("projectId"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "delete project")
		return
	}
	// Re-render every affected server now — without the nudge the cascaded
	// resources kept running until the 60s fleet resync (SIGMA-193).
	if s.reconcile != nil {
		for _, sid := range servers {
			s.reconcile.ReconcileAsync(orgID, sid)
		}
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

// handleUpdateEnvironment edits an environment's production flag (SIGMA-190 —
// previously write-once at creation, inferred web-side from a magic name).
func (s *Server) handleUpdateEnvironment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Production *bool `json:"production"`
	}
	if err := decodeJSON(w, r, &req); err != nil || req.Production == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "production (boolean) is required"})
		return
	}
	env, err := s.domain.UpdateEnvironmentProduction(r.Context(),
		r.PathValue("orgId"), r.PathValue("envId"), *req.Production, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "update environment")
		return
	}
	writeJSON(w, http.StatusOK, env)
}

func (s *Server) handleDeleteEnvironment(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	servers, err := s.domain.DeleteEnvironment(r.Context(), orgID, r.PathValue("envId"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "delete environment")
		return
	}
	// Same nudge as project delete (SIGMA-193).
	if s.reconcile != nil {
		for _, sid := range servers {
			s.reconcile.ReconcileAsync(orgID, sid)
		}
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
		EnvironmentID string `json:"environmentId"`
		ServerID      string `json:"serverId"`
		// ClusterID deploys INTO a Kubernetes cluster instead of onto one server.
		// The store has supported it since clusters shipped, but nothing outside
		// the process could ever set it: this handler didn't decode the field and
		// refused anything without a serverId, so the whole cluster render path
		// was unreachable over HTTP (SIGMA-200).
		ClusterID string          `json:"clusterId"`
		Name      string          `json:"name"`
		Kind      string          `json:"kind"`
		Spec      json.RawMessage `json:"spec"`
	}
	if err := decodeJSON(w, r, &req); err != nil ||
		strings.TrimSpace(req.Name) == "" || req.Kind == "" || req.EnvironmentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "environmentId, name and kind are required"})
		return
	}
	req.ServerID, req.ClusterID = strings.TrimSpace(req.ServerID), strings.TrimSpace(req.ClusterID)
	// Exactly one target. The old check was `req.ServerID == ""` alone, so a
	// correct cluster deploy came back "serverId is required" — an error that
	// describes the shape the handler happened to expect rather than what is
	// wrong with the request, and that reads as a client bug when the client did
	// nothing wrong. Name the actual mistake in both directions.
	switch {
	case req.ServerID == "" && req.ClusterID == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "a deploy target is required: pass serverId to run this on one server, or clusterId to run it in a cluster"})
		return
	case req.ServerID != "" && req.ClusterID != "":
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "serverId and clusterId are mutually exclusive: a resource runs on one server or inside a cluster, not both"})
		return
	}
	// The cluster exclusion is a domain rule, not a request shape, so it answers
	// 422 with the reason. The store enforces it again inside the create
	// transaction — this is the copy that gets to say it before a round trip.
	if req.ClusterID != "" && !store.ClusterKindAllowed(req.Kind) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": store.ErrKindNotClusterable{Kind: req.Kind}.Error()})
		return
	}
	res, err := s.domain.CreateResource(r.Context(), r.PathValue("orgId"), store.CreateResourceInput{
		EnvironmentID: req.EnvironmentID,
		ServerID:      req.ServerID,
		ClusterID:     req.ClusterID,
		Name:          strings.TrimSpace(req.Name),
		Kind:          req.Kind,
		Spec:          req.Spec,
	}, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "create resource")
		return
	}
	// Re-render whatever actually applies this resource. A server resource is its
	// own server's business; a cluster workload renders only into the
	// control-plane node's document and the resource carries no server_id to
	// point at, so it has to be looked up.
	if s.reconcile != nil {
		switch {
		case res.ServerID != "":
			s.reconcile.ReconcileAsync(res.OrgID, res.ServerID)
		case req.ClusterID != "":
			cp, cerr := s.domain.ControlPlaneServerForCluster(r.Context(), res.OrgID, req.ClusterID)
			if cerr != nil {
				// Non-fatal: the resource exists and the 60s fleet resync will pick
				// it up. Only the immediacy is lost, so don't fail a good create.
				s.log.Warn("cluster workload created but its control-plane node could not be re-rendered",
					"cluster", req.ClusterID, "err", cerr)
			} else {
				s.reconcile.ReconcileAsync(res.OrgID, cp)
			}
		}
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
	orgID := r.PathValue("orgId")
	serverID, err := s.domain.DeleteResource(r.Context(), orgID, r.PathValue("resourceId"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "delete resource")
		return
	}
	if s.reconcile != nil && serverID != "" {
		s.reconcile.ReconcileAsync(orgID, serverID)
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
	orgID, serverID := r.PathValue("orgId"), r.PathValue("serverId")
	err := s.domain.SetProxyRole(r.Context(), orgID, serverID, req.Proxy, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "proxy role")
		return
	}
	// The proxy role gates the Traefik op AND (via P1-5) the 80/443 firewall
	// rules — both live in the server's DSD, so re-render it.
	if s.reconcile != nil {
		s.reconcile.ReconcileAsync(orgID, serverID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "proxy": req.Proxy})
}

// ── Custom domains (P1-8) ───────────────────────────────────────────────────

func (s *Server) handleAttachDomain(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Domain        string `json:"domain"`
		ChallengeType string `json:"challengeType"`
	}
	if err := decodeJSON(w, r, &req); err != nil || strings.TrimSpace(req.Domain) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "domain is required"})
		return
	}
	orgID := r.PathValue("orgId")
	resourceID := r.PathValue("resourceId")
	d, serverID, err := s.domain.AttachDomain(r.Context(), orgID, resourceID,
		req.Domain, req.ChallengeType, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "attach domain")
		return
	}
	// A new domain adds Traefik router labels to the resource's container. The
	// labels only reach a LIVE container through a fresh rollout generation, so
	// mint a config deployment (SIGMA-166) before the re-render — without it the
	// agent's never-cut guard refused the changed spec and the hostname never
	// routed.
	s.mintConfigDeploys(r, orgID, []string{resourceID}, "domain attached")
	if s.reconcile != nil && serverID != "" {
		s.reconcile.ReconcileAsync(orgID, serverID)
	}
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) handleListDomains(w http.ResponseWriter, r *http.Request) {
	domains, err := s.domain.ListDomainsForResource(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId"))
	if err != nil {
		s.writeStoreErr(w, err, "list domains")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"domains": domains})
}

func (s *Server) handleDetachDomain(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	serverID, resourceID, err := s.domain.DetachDomain(r.Context(), orgID, r.PathValue("domainId"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "detach domain")
		return
	}
	// Removing router labels changes the rendered spec — same generation rule
	// as attach (SIGMA-166).
	if resourceID != "" {
		s.mintConfigDeploys(r, orgID, []string{resourceID}, "domain detached")
	}
	if s.reconcile != nil && serverID != "" {
		s.reconcile.ReconcileAsync(orgID, serverID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "detached"})
}

// handleSetHardening updates a server's desired hardening config (the
// keep-public-SSH opt-out, CIS, and inbound exceptions). The change re-renders
// the host.* DSD ops on the next reconcile. Project Admin+; audited.
func (s *Server) handleSetHardening(w http.ResponseWriter, r *http.Request) {
	var req struct {
		KeepPublicSSH bool `json:"keepPublicSsh"`
		CISEnabled    bool `json:"cisEnabled"`
		ExtraPorts    []struct {
			Port  int    `json:"port"`
			Proto string `json:"proto"`
		} `json:"extraPorts"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	ports := make([]store.PortException, 0, len(req.ExtraPorts))
	for _, p := range req.ExtraPorts {
		if p.Port <= 0 || p.Port > 65535 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid port"})
			return
		}
		ports = append(ports, store.PortException{Port: p.Port, Proto: p.Proto})
	}
	orgID, serverID := r.PathValue("orgId"), r.PathValue("serverId")
	if err := s.domain.SetHardeningConfig(r.Context(), orgID, serverID, req.KeepPublicSSH, req.CISEnabled, ports, principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "set hardening")
		return
	}
	if s.reconcile != nil {
		s.reconcile.ReconcileAsync(orgID, serverID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
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
		"orgId":    req.OrgID,
		"token":    tok,
		"tokenId":  p.ID,
		"role":     string(p.Role),
		"issuedAt": time.Now().UTC(),
	})
}

// handleGetLLM returns a model endpoint's readout (CP mode, llm kind only).
func (s *Server) handleGetLLM(w http.ResponseWriter, r *http.Request) {
	if s.llm == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "model hosting is not configured"})
		return
	}
	info, err := s.llm.GetLLM(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId"))
	if err != nil {
		s.writeStoreErr(w, err, "llm endpoint")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleListLLMEngines publishes the supported inference runtimes so the
// dashboard offers exactly what the control plane can render.
func (s *Server) handleListLLMEngines(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"engines": store.LLMEngineNames(),
		"default": store.DefaultLLMEngine,
	})
}

// handleDomainDNS returns the DNS records a custom domain needs plus a live
// verification, so "why isn't my domain working" is answerable in the product
// instead of requiring dig.
func (s *Server) handleDomainDNS(w http.ResponseWriter, r *http.Request) {
	if s.dns == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "dns setup is not configured"})
		return
	}
	setup, err := s.dns.DNSSetupForDomain(r.Context(), r.PathValue("orgId"), r.PathValue("domainId"))
	if err != nil {
		s.writeStoreErr(w, err, "dns setup")
		return
	}
	writeJSON(w, http.StatusOK, setup)
}
