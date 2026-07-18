// Package api is the control-plane HTTP surface.
package api

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/paddle"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/telemetry"
)

// Pinger is the slice of the store the API needs for readiness.
type Pinger interface {
	Ping(ctx context.Context) error
}

// StoreAPI is everything the HTTP handlers need from the store; faked in
// tests.
type StoreAPI interface {
	IssueBootstrapToken(ctx context.Context, orgID, serverName, serverType, provider, region, createdBy string, ttl time.Duration) (string, string, time.Time, error)
	ProvisionServer(ctx context.Context, orgID string, in store.ProvisionInput, createdBy string, ttl time.Duration) (store.ProvisionResult, error)
	RegisterServer(ctx context.Context, bootstrapToken, name, agentVersion string, facts json.RawMessage, pubkey string) (store.RegisterResult, error)
	ServerByAgentToken(ctx context.Context, token string) (store.Server, error)
	AuthenticateServiceToken(ctx context.Context, token string) (store.ServicePrincipal, error)
	RecordHeartbeat(ctx context.Context, serverID string, in store.HeartbeatInput) error
	MeshPeers(ctx context.Context, orgID, selfServerID string) ([]store.MeshPeer, error)
	MetricsSince(ctx context.Context, orgID, serverID string, since time.Time) ([]store.MetricPoint, error)
	ListServers(ctx context.Context, orgID string) ([]store.Server, error)
	GetServer(ctx context.Context, orgID, serverID string) (store.Server, error)
	ResolveSecretsForResource(ctx context.Context, orgID, serverID, resourceID, actor string) ([]store.ResolvedSecret, error)
	SetDomainCertStatus(ctx context.Context, serverID, domain, status, serial string, expiresAt *time.Time, certErr string) error
	DeploymentCloneCredential(ctx context.Context, serverID, deploymentID string) (token, repo, provider string, err error)
	AdvanceDeploymentForResource(ctx context.Context, serverID, resourceID, phase string, ok bool, detail string) error
	AdvanceDeploymentService(ctx context.Context, serverID, resourceID, service, phase string, ok bool, detail string) error
	AppendDeployLog(ctx context.Context, serverID, deploymentID, stream, line string) error
	// Backups (P1-11): the audited per-run credential release and the agent's
	// terminal result report, plus the op-status failure fallback.
	BackupCredentialForRun(ctx context.Context, serverID, runID string) (store.BackupCredential, error)
	// WAL shipping (P2-5).
	WALTargetsForServer(ctx context.Context, serverID string) ([]store.WALTarget, error)
	WALCredentialForResource(ctx context.Context, serverID, resourceID string) (store.BackupCredential, error)
	SetWALStatus(ctx context.Context, serverID, resourceID, lastSegment string, at time.Time) error
	SetBackupRunResult(ctx context.Context, serverID, runID string, ok bool, snapshotID, dumpSha, detail string) error
	FailBackupRunFromOpStatus(ctx context.Context, serverID, runID, errText string) error
}

// ReconcileTrigger nudges the reconciler after a resource mutation.
type ReconcileTrigger interface {
	ReconcileAsync(orgID, serverID string)
}

type Server struct {
	log                 *slog.Logger
	db                  Pinger
	store               StoreAPI
	domain              DomainAPI
	git                 GitAPI
	inspector           RepoInspector
	installTokens       InstallationTokenSource
	githubAppSlug       string
	dsdStore            DSDStore
	dsdWaiter           DSDWaiter
	reconcile           ReconcileTrigger
	dsdPub              ed25519.PublicKey
	devServiceToken     string
	provisionToken      string
	githubWebhookSecret string
	// telemetry forwards metrics/logs to VictoriaMetrics/Loki and proxies
	// tenant-isolated queries (P1-13); tel is its store slice. Nil in handler
	// unit tests → telemetry endpoints answer "not configured".
	telemetry *telemetry.Forwarder
	tel       TelemetryAPI
	// alertSender test-fires alert channels (P2-6); nil → test endpoint 503s.
	alertSender AlertSender
	// Billing (P2-4). billing is the store slice; paddle is the outbound
	// client (nil = billing off / honest not-configured); paddleWebhookSecret
	// empty = the webhook receiver 503s; paddlePriceID is the checkout price.
	billing             BillingStore
	paddle              PaddleClient
	paddleWebhookSecret string
	paddlePriceID       string
	mux                 *http.ServeMux
}

// PaddleClient is the outbound Paddle surface the billing handlers need.
type PaddleClient interface {
	CreateCheckout(ctx context.Context, in paddle.CreateTransactionInput) (paddle.Transaction, error)
	CustomerPortalURL(ctx context.Context, customerID string) (string, error)
}

// Options carries the API's authn material and DSD runtime dependencies.
// DevServiceToken is the dev-mode static bypass (empty in prod); ProvisionToken
// gates POST /v1/orgs. The DSD* fields are nil in handler unit tests that don't
// exercise the agent DSD routes.
type Options struct {
	DevServiceToken string
	ProvisionToken  string
	// Git is the git-integration store slice (P1-7). Nil in handler unit tests
	// that don't exercise the git routes.
	Git GitAPI
	// Inspector reads a connected repo's files to derive the deploy config. Nil
	// disables detection (the detect endpoint 503s and connect skips its gate).
	Inspector RepoInspector
	// InstallationTokens mints GitHub App installation tokens (SIGMA-55); nil
	// when no App is configured — detect/connect then rely on pasted PATs.
	// GitHubAppSlug is the registered App's slug, so the dashboard can build
	// the https://github.com/apps/<slug>/installations/new install link.
	InstallationTokens InstallationTokenSource
	GitHubAppSlug      string
	// GitHubWebhookSecret verifies inbound deliveries; empty 503s the receiver.
	GitHubWebhookSecret string
	DSDStore            DSDStore
	DSDWaiter           DSDWaiter
	Reconcile           ReconcileTrigger
	DSDPublicKey        ed25519.PublicKey
	// Telemetry is the P1-13 forwarder (nil disables the pipeline endpoints);
	// TelemetryStore is its store slice.
	Telemetry      *telemetry.Forwarder
	TelemetryStore TelemetryAPI
	// AlertSender test-fires alert channels (P2-6); nil in handler unit tests.
	AlertSender AlertSender
	// Billing (P2-4). Billing is the store slice; Paddle is the outbound client
	// (nil = billing off); PaddleWebhookSecret/PaddlePriceID configure the
	// receiver + checkout.
	Billing             BillingStore
	Paddle              PaddleClient
	PaddleWebhookSecret string
	PaddlePriceID       string
}

// New builds the HTTP surface.
func New(log *slog.Logger, db Pinger, st StoreAPI, dom DomainAPI, opts Options) *Server {
	s := &Server{
		log: log, db: db, store: st, domain: dom,
		git:                 opts.Git,
		inspector:           opts.Inspector,
		installTokens:       opts.InstallationTokens,
		githubAppSlug:       opts.GitHubAppSlug,
		dsdStore:            opts.DSDStore,
		dsdWaiter:           opts.DSDWaiter,
		reconcile:           opts.Reconcile,
		dsdPub:              opts.DSDPublicKey,
		devServiceToken:     opts.DevServiceToken,
		provisionToken:      opts.ProvisionToken,
		githubWebhookSecret: opts.GitHubWebhookSecret,
		telemetry:           opts.Telemetry,
		tel:                 opts.TelemetryStore,
		alertSender:         opts.AlertSender,
		billing:             opts.Billing,
		paddle:              opts.Paddle,
		paddleWebhookSecret: opts.PaddleWebhookSecret,
		paddlePriceID:       opts.PaddlePriceID,
		mux:                 http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)

	// Org provisioning (dedicated token; mints the org's web credential).
	s.mux.HandleFunc("POST /v1/orgs", s.requireProvision(s.handleProvisionOrg))

	// Git provider webhook (P1-7). Public: unauthenticated but HMAC-verified
	// against the configured secret; a forged signature is rejected 401.
	s.mux.HandleFunc("POST /v1/webhooks/github", s.handleGitHubWebhook)
	// Paddle billing webhook (P2-4). Public: Paddle-Signature HMAC-verified;
	// 503 when no secret is configured.
	s.mux.HandleFunc("POST /v1/webhooks/paddle", s.handlePaddleWebhook)

	// Dashboard-facing (org-scoped service tokens): reads need any role,
	// mutations need at least Project Admin — mirroring the v1 web RBAC.
	// Mutating POSTs support Idempotency-Key replay — EXCEPT token minting:
	// replaying a mint must issue a fresh token, never store/return the
	// one-time plaintext for later replay.
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/bootstrap-tokens", s.requireService(store.RoleProjectAdmin, s.handleIssueBootstrapToken))
	// SSH onboarding (P1-5): pre-create the server + mint a per-server bootstrap
	// keypair. Project Admin+; like token minting, not idempotency-replayable.
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/servers/provision", s.requireService(store.RoleProjectAdmin, s.handleProvisionServer))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/servers", s.requireService(store.RoleDeveloper, s.handleListServers))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/servers/{serverId}", s.requireService(store.RoleDeveloper, s.handleGetServer))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/servers/{serverId}/metrics", s.requireService(store.RoleDeveloper, s.handleGetMetrics))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/servers/{serverId}/proxy-role", s.requireService(store.RoleProjectAdmin, s.handleProxyRole))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/servers/{serverId}/hardening", s.requireService(store.RoleProjectAdmin, s.handleSetHardening))
	// Server + token lifecycle (P1-4). Server delete and agent-token revoke are
	// Project Admin+; service-token lifecycle is Org Admin only.
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/servers/{serverId}", s.requireService(store.RoleProjectAdmin, s.handleDeleteServer))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/servers/{serverId}/revoke-token", s.requireService(store.RoleProjectAdmin, s.handleRevokeAgentToken))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/service-tokens", s.requireService(store.RoleOrgAdmin, s.handleListServiceTokens))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/service-tokens/{tokenId}", s.requireService(store.RoleOrgAdmin, s.handleRevokeServiceToken))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/service-tokens/{tokenId}/rotate", s.requireService(store.RoleOrgAdmin, s.handleRotateServiceToken))
	// Two-phase destructive-op confirm (P1-3). Not idempotency-wrapped: a
	// replayed mint must issue a fresh single-use token, never replay a stored
	// one. Both phases are Project Admin+ and audited by the store.
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/servers/{serverId}/confirm-tokens", s.requireService(store.RoleProjectAdmin, s.handleIssueConfirmToken))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/servers/{serverId}/destructive-ops", s.requireService(store.RoleProjectAdmin, s.handleConfirmDestructive))

	// Domain model (P1-1).
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/projects", s.requireService(store.RoleProjectAdmin, s.idempotent(s.handleCreateProject)))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/projects", s.requireService(store.RoleDeveloper, s.handleListProjects))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/projects/{projectId}", s.requireService(store.RoleDeveloper, s.handleGetProject))
	s.mux.HandleFunc("PATCH /v1/orgs/{orgId}/projects/{projectId}", s.requireService(store.RoleProjectAdmin, s.handleUpdateProject))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/projects/{projectId}", s.requireService(store.RoleProjectAdmin, s.handleDeleteProject))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/projects/{projectId}/environments", s.requireService(store.RoleProjectAdmin, s.idempotent(s.handleCreateEnvironment)))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/projects/{projectId}/environments", s.requireService(store.RoleDeveloper, s.handleListEnvironments))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/environments/{envId}", s.requireService(store.RoleProjectAdmin, s.handleDeleteEnvironment))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/environments/{envId}/servers", s.requireService(store.RoleProjectAdmin, s.idempotent(s.handleAttachServer)))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/environments/{envId}/servers", s.requireService(store.RoleDeveloper, s.handleEnvServers))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/environments/{envId}/servers/{serverId}", s.requireService(store.RoleProjectAdmin, s.handleDetachServer))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/resources", s.requireService(store.RoleProjectAdmin, s.idempotent(s.handleCreateResource)))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/resources", s.requireService(store.RoleDeveloper, s.handleListResources))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/resources/{resourceId}", s.requireService(store.RoleProjectAdmin, s.handleDeleteResource))

	// Database resources (P1-10). Metadata is member-visible; the credential
	// reveal is Project Admin+ (a Developer 403s) and audited; the public-
	// exposure hook returns the typed not-enabled error (mesh-only v1).
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/resources/{resourceId}/database", s.requireService(store.RoleDeveloper, s.handleGetDatabase))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/resources/{resourceId}/database/connection", s.requireService(store.RoleProjectAdmin, s.handleRevealDatabaseConnection))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/resources/{resourceId}/database/expose", s.requireService(store.RoleProjectAdmin, s.handleExposeDatabase))
	// S3 storage (P2-1): same split as databases.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/resources/{resourceId}/s3", s.requireService(store.RoleDeveloper, s.handleGetS3))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/resources/{resourceId}/s3/connection", s.requireService(store.RoleProjectAdmin, s.handleRevealS3Connection))

	// Backups (P1-11). Target metadata + run history + the verify-day feed are
	// member-visible; target lifecycle, policy edits and the fire-drill restore
	// are Project Admin+. Target creation carries credentials, so like token
	// minting it is deliberately not idempotency-replayable.
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/backup-targets", s.requireService(store.RoleProjectAdmin, s.handleCreateBackupTarget))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/backup-targets", s.requireService(store.RoleDeveloper, s.handleListBackupTargets))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/backup-targets/{targetId}", s.requireService(store.RoleProjectAdmin, s.handleDeleteBackupTarget))
	s.mux.HandleFunc("PATCH /v1/orgs/{orgId}/resources/{resourceId}/backup-policy", s.requireService(store.RoleProjectAdmin, s.handleUpdateBackupPolicy))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/resources/{resourceId}/backup-runs", s.requireService(store.RoleDeveloper, s.handleListBackupRuns))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/backups/verify-days", s.requireService(store.RoleDeveloper, s.handleVerifyDays))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/resources/{resourceId}/restore", s.requireService(store.RoleProjectAdmin, s.idempotent(s.handleRestoreDatabase)))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/resources/{resourceId}/restore-pitr", s.requireService(store.RoleProjectAdmin, s.idempotent(s.handleRestoreToTimestamp)))

	// Custom domains (P1-8): attach/detach are Project Admin+, listing is member-visible.
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/resources/{resourceId}/domains", s.requireService(store.RoleProjectAdmin, s.handleAttachDomain))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/resources/{resourceId}/domains", s.requireService(store.RoleDeveloper, s.handleListDomains))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/domains/{domainId}", s.requireService(store.RoleProjectAdmin, s.handleDetachDomain))

	// Deployments (P1-9): release history + build-log stream are member-visible;
	// a rollback (re-ships a prior release) is Project Admin+.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/resources/{resourceId}/deployments", s.requireService(store.RoleDeveloper, s.handleListDeployments))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/resources/{resourceId}/rollback-targets", s.requireService(store.RoleDeveloper, s.handleRollbackTargets))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/resources/{resourceId}/rollback", s.requireService(store.RoleProjectAdmin, s.handleRollback))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/resources/{resourceId}/deploy", s.requireService(store.RoleProjectAdmin, s.handleRedeploy))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/deployments/{deploymentId}/logs", s.requireService(store.RoleDeveloper, s.handleDeployLogs))
	// Audit is member-visible (matches the web settings tab shown to all
	// members); mutations above stay Project Admin+.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/audit", s.requireService(store.RoleDeveloper, s.handleListAudit))

	// Telemetry (P1-13): tenant-isolated PromQL/LogQL proxies for the embedded
	// dashboards + the M1 beta-metrics feed. All member-visible reads.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/metrics/query", s.requireService(store.RoleDeveloper, s.handleMetricsQuery))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/logs/query", s.requireService(store.RoleDeveloper, s.handleLogsQuery))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/beta-metrics", s.requireService(store.RoleDeveloper, s.handleBetaMetrics))

	// Git integration (P1-7). Detection is a read-only preview (Developer+);
	// connecting a repo and editing branch routes/policies are Project Admin+.
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/git/detect", s.requireService(store.RoleDeveloper, s.handleGitDetect))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/git/connections", s.requireService(store.RoleProjectAdmin, s.handleGitConnect))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/git/connections", s.requireService(store.RoleDeveloper, s.handleListGitConnections))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/git/connections/{connId}", s.requireService(store.RoleDeveloper, s.handleGetGitConnection))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/git/connections/{connId}", s.requireService(store.RoleProjectAdmin, s.handleDeleteGitConnection))
	s.mux.HandleFunc("PUT /v1/orgs/{orgId}/git/connections/{connId}/branches", s.requireService(store.RoleProjectAdmin, s.handleSetBranchMap))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/git/branch-maps/{mapId}", s.requireService(store.RoleProjectAdmin, s.handleDeleteBranchMap))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/git/branch-maps/{mapId}/promote", s.requireService(store.RoleProjectAdmin, s.handlePromoteBranch))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/git/deploy-requests", s.requireService(store.RoleDeveloper, s.handleListDeployRequests))
	// GitHub App (SIGMA-55): install-link metadata is member-visible; linking
	// an installation to a connection mutates it, so Project Admin+.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/git/app", s.requireService(store.RoleDeveloper, s.handleGitAppInfo))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/git/connections/{connId}/installation", s.requireService(store.RoleProjectAdmin, s.handleSetInstallation))
	// Previews (P1-12): the per-connection toggle is Project Admin+; the PR
	// environment list is member-visible.
	s.mux.HandleFunc("PUT /v1/orgs/{orgId}/git/connections/{connId}/previews", s.requireService(store.RoleProjectAdmin, s.handleSetPreviews))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/git/connections/{connId}/previews", s.requireService(store.RoleDeveloper, s.handleListPreviews))

	// Alerting (P2-6): channel CRUD/rules/test are Org Admin (org-wide
	// notification wiring); the list is member-visible metadata.
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/alert-channels", s.requireService(store.RoleOrgAdmin, s.idempotent(s.handleCreateAlertChannel)))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/alert-channels", s.requireService(store.RoleDeveloper, s.handleListAlertChannels))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/alert-channels/{channelId}", s.requireService(store.RoleOrgAdmin, s.handleDeleteAlertChannel))
	s.mux.HandleFunc("PUT /v1/orgs/{orgId}/alert-channels/{channelId}/rules", s.requireService(store.RoleOrgAdmin, s.handleSetAlertRules))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/alert-channels/{channelId}/test", s.requireService(store.RoleOrgAdmin, s.handleTestAlertChannel))

	// Billing (P2-4). The usage+charge summary is member-visible; checkout and
	// the customer portal mutate the subscription so they need Org Admin.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/billing", s.requireService(store.RoleDeveloper, s.handleGetBilling))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/billing/checkout", s.requireService(store.RoleOrgAdmin, s.handleBillingCheckout))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/billing/portal", s.requireService(store.RoleOrgAdmin, s.handleBillingPortal))

	// Secrets (P1-6). List is Developer+ (metadata only); create/delete need
	// Project Admin; raw-value reveal needs Project Admin (Developer 403s);
	// KEK/DEK rotation is Org Admin only.
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/projects/{projectId}/secrets", s.requireService(store.RoleProjectAdmin, s.idempotent(s.handleCreateSecret)))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/projects/{projectId}/secrets", s.requireService(store.RoleDeveloper, s.handleListSecrets))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/secrets/{secretId}/value", s.requireService(store.RoleProjectAdmin, s.handleRevealSecret))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/secrets/{secretId}", s.requireService(store.RoleProjectAdmin, s.handleDeleteSecret))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/secrets/rotate-kek", s.requireService(store.RoleOrgAdmin, s.handleRotateKEK))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/secrets/rotate-dek", s.requireService(store.RoleOrgAdmin, s.handleRotateDEK))

	// Agent-facing.
	s.mux.HandleFunc("POST /v1/agent/register", s.handleRegister)
	s.mux.HandleFunc("POST /v1/agent/heartbeat", s.requireAgent(s.handleHeartbeat))
	s.mux.HandleFunc("GET /v1/agent/mesh/peers", s.requireAgent(s.handleMeshPeers))
	s.mux.HandleFunc("GET /v1/agent/dsd", s.requireAgent(s.handleGetDSD))
	s.mux.HandleFunc("POST /v1/agent/dsd/status", s.requireAgent(s.handleDSDStatus))
	s.mux.HandleFunc("GET /v1/agent/secrets", s.requireAgent(s.handleAgentSecrets))
	s.mux.HandleFunc("POST /v1/agent/domains/status", s.requireAgent(s.handleAgentDomainStatus))
	s.mux.HandleFunc("GET /v1/agent/git-credential", s.requireAgent(s.handleAgentGitCredential))
	s.mux.HandleFunc("POST /v1/agent/build-logs", s.requireAgent(s.handleAgentBuildLog))
	s.mux.HandleFunc("GET /v1/agent/backup-credential", s.requireAgent(s.handleAgentBackupCredential))
	s.mux.HandleFunc("POST /v1/agent/backup-status", s.requireAgent(s.handleAgentBackupStatus))
	// WAL shipping (P2-5).
	s.mux.HandleFunc("GET /v1/agent/wal-targets", s.requireAgent(s.handleAgentWALTargets))
	s.mux.HandleFunc("GET /v1/agent/wal-credential", s.requireAgent(s.handleAgentWALCredential))
	s.mux.HandleFunc("POST /v1/agent/wal-status", s.requireAgent(s.handleAgentWALStatus))
	s.mux.HandleFunc("POST /v1/agent/telemetry/metrics", s.requireAgent(s.handleAgentTelemetryMetrics))
	s.mux.HandleFunc("POST /v1/agent/telemetry/logs", s.requireAgent(s.handleAgentTelemetryLogs))
}

func (s *Server) Handler() http.Handler {
	return s.withLogging(s.mux)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable", "reason": "database unreachable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// maxBodyBytes caps request bodies. The register endpoint is unauthenticated,
// so an unbounded decode is a pre-auth memory-exhaustion vector.
const maxBodyBytes = 1 << 20 // 1 MiB

// decodeJSON reads a size-capped JSON body. On an oversized or malformed body
// it returns an error; callers respond 400.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	return json.NewDecoder(r.Body).Decode(dst)
}
