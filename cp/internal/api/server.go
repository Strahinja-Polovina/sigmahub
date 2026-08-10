// Package api is the control-plane HTTP surface.
package api

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/cpmetrics"
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
	ReissueBootstrapToken(ctx context.Context, orgID, serverID, createdBy string, ttl time.Duration) (store.ProvisionResult, error)
	SetDesiredAgentVersion(ctx context.Context, orgID, serverID, version, actor string) error
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
	AdvanceDeploymentForResource(ctx context.Context, serverID, resourceID, phase string, ok bool, detail string, reportVersion int64) error
	AdvanceDeploymentService(ctx context.Context, serverID, resourceID, service, phase string, ok bool, detail string, reportVersion int64) error
	// FailDeploymentFromPrereqOp routes a phase-less pipeline op failure
	// (image.pull, volume.ensure, per-resource network.ensure) into the deploy log
	// and the deployment's terminal detail (SIGMA-301).
	FailDeploymentFromPrereqOp(ctx context.Context, serverID, resourceID, opID, errText string, reportVersion int64) error
	// DeployPeersForResource lists the other servers whose documents are gated
	// on this resource's deployment status, so a multi-machine pipeline advances
	// on the report rather than on the next resync.
	DeployPeersForResource(ctx context.Context, resourceID, excludeServerID string) ([]store.ServerRef, error)
	// AppendDeployLogs takes the whole batch the agent posted and writes it in
	// one statement — one INSERT per line was thousands of statements per build
	// (SIGMA-252).
	AppendDeployLogs(ctx context.Context, serverID, deploymentID, stream string, lines []string) error
	// Backups (P1-11): the audited per-run credential release and the agent's
	// terminal result report, plus the op-status failure fallback.
	BackupCredentialForRun(ctx context.Context, serverID, runID string) (store.BackupCredential, error)
	// WAL shipping (P2-5).
	WALTargetsForServer(ctx context.Context, serverID string) ([]store.WALTarget, error)
	WALCredentialForResource(ctx context.Context, serverID, resourceID string) (store.BackupCredential, error)
	SetWALStatus(ctx context.Context, serverID, resourceID, lastSegment string, at time.Time) error
	SetBackupRunResult(ctx context.Context, serverID, runID string, ok bool, snapshotID, dumpSha, detail string) error
	FailBackupRunFromOpStatus(ctx context.Context, serverID, runID, errText string) error
	// S3 bucket/key/quota ops (SIGMA-65): the audited per-op credential release,
	// the agent's terminal status report, and the DSD op-status failure fallback.
	S3OpCredentialForOp(ctx context.Context, serverID, opID string) (store.S3OpCredential, error)
	MarkS3OpApplied(ctx context.Context, serverID, opID, detail string) error
	MarkS3OpFailed(ctx context.Context, serverID, opID, detail string) error
	RecordStorageBytes(ctx context.Context, serverID, opID string, bytes int64, now time.Time) error
	FailS3OpFromOpStatus(ctx context.Context, serverID, opID, errText string) error
	// CompleteDecommission finishes a graceful decommission on the agent's ack
	// (SIGMA-204). Keyed by server id alone: the caller is the agent-token
	// handler, which already resolved the server from the credential.
	CompleteDecommission(ctx context.Context, serverID string, ok bool, detail string) error
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
	gitIntegration      GitIntegrationAPI
	compose             ComposeAPI
	clusters            ClusterAPI
	registry            RegistryAPI
	llm                 LLMAPI
	models              ModelCatalog
	dns                 DNSAPI
	inspector           RepoInspector
	repoLister          RepoLister
	installTokens       InstallationTokenSource
	installAccounts     InstallationAccountSource
	githubAppSlug       string
	dsdStore            DSDStore
	dsdWaiter           DSDWaiter
	reconcile           ReconcileTrigger
	dsdPub              ed25519.PublicKey
	devServiceToken     string
	provisionToken      string
	githubWebhookSecret string
	publicURL           string
	// dbEngines/s3Engines are the enabled-engine allowlists (CP_DB_ENGINES,
	// CP_S3_ENGINES), published by GET /v1/orgs/{orgId}/capabilities so the
	// wizard can stop offering an engine this deployment turned off. Empty
	// means "not restricted" — see enabledOrAll.
	dbEngines []string
	s3Engines []string
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
	// requireActor rejects org-scoped tokens presented with no actor header
	// (SIGMA-82); off by default. The dev wildcard token is exempt.
	requireActor bool
	// release is where GET /install.sh and GET /dl/{version}/{asset} fetch from,
	// and the only place the release credential lives. Zero value (no Repo) =
	// both routes answer 503 naming the setting. See installer.go.
	release ReleaseSource
	// metrics is the control plane's own health registry (SIGMA-248). Always
	// non-nil after New — a *cpmetrics.Registry with no reporters still names
	// every loop, and an endpoint that exists unconditionally is worth more than
	// one that quietly disappears in the deployments that forgot to wire it.
	metrics *cpmetrics.Registry
	// metricsRetentionCfg is how long the sweeper keeps server_metrics rows;
	// the fallback metrics path may not advertise a longer window than that
	// (SIGMA-257). Zero = the built-in default; read via metricsRetention().
	metricsRetentionCfg time.Duration
	// orgAdmin backs the tombstone check that stops a purged org id being
	// provisioned again (SIGMA-298). The erasure itself is DomainAPI.PurgeOrg
	// (SIGMA-284) — the two tickets each built a delete engine, and this is the
	// half of SIGMA-298 that PurgeOrg did not already have.
	orgAdmin OrgAdminAPI
	mux      *http.ServeMux
}

// PaddleClient is the outbound Paddle surface the billing handlers need.
type PaddleClient interface {
	CreateCheckout(ctx context.Context, in paddle.CreateTransactionInput) (paddle.Transaction, error)
	CustomerPortalURL(ctx context.Context, customerID string) (string, error)
	// SetSubscriptionOrg stamps orgId into the subscription's custom_data so
	// later events on it correlate through the primary path (SIGMA-293).
	SetSubscriptionOrg(ctx context.Context, subscriptionID, orgID string) error
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
	// GitIntegration backs the org-level GitHub integration (connect once,
	// then pick repos). Nil → those endpoints answer "not configured".
	GitIntegration GitIntegrationAPI
	// RepoLister lists the repos an installation grants — the picker's source.
	RepoLister RepoLister
	// Compose backs per-service placement for multi-service apps.
	Compose ComposeAPI
	// Clusters backs Kubernetes cluster setup and membership.
	Clusters ClusterAPI
	// Registry backs the org's container image registry — what makes a build on
	// one machine runnable on another.
	Registry RegistryAPI
	// LLM backs GPU model-hosting endpoints.
	LLM LLMAPI
	// Models backs the Hugging Face model picker (SIGMA-213). Nil is a
	// SUPPORTED state, not a broken one: search then answers an empty catalog
	// with tokenConfigured=false, which the wizard renders as a free-text model
	// field. Nothing here may 503 — see handleSearchModels.
	Models ModelCatalog
	// DNS derives and verifies the records a custom domain needs.
	DNS DNSAPI
	// InstallationAccounts names an installation's account for the dashboard.
	InstallationAccounts InstallationAccountSource
	// GitHubWebhookSecret verifies inbound deliveries; empty 503s the receiver.
	GitHubWebhookSecret string
	// PublicURL is the CP's own public base URL (e.g. https://cp.example.com).
	// With GitHubWebhookSecret set, connecting a repo auto-registers the
	// push-to-deploy webhook pointing at <PublicURL>/v1/webhooks/github.
	PublicURL string
	// DBEngines/S3Engines are the enabled-engine allowlists this control plane
	// was configured with (config.DBEngines / config.S3Engines — the same
	// values handed to the store). Empty means unrestricted. They are
	// PUBLISHED, at GET /v1/orgs/{orgId}/capabilities, because the dashboard's
	// wizard is built from the generated catalog and had no way to know which
	// engines a given control plane turned off (SIGMA-268).
	DBEngines    []string
	S3Engines    []string
	DSDStore     DSDStore
	DSDWaiter    DSDWaiter
	Reconcile    ReconcileTrigger
	DSDPublicKey ed25519.PublicKey
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
	// RequireActor makes a valid actor header mandatory on org-scoped tokens
	// (SIGMA-82); off by default (the dev wildcard token is always exempt).
	RequireActor bool
	// Release configures the two unauthenticated installer routes: which
	// repository's releases they serve, which tag GET /install.sh is pinned to,
	// and the server-side GitHub credential that makes a PRIVATE release
	// repository onboardable. Zero value = the routes answer 503 rather than
	// guessing a repository. See installer.go for the whole security argument.
	Release ReleaseSource
	// Metrics is the control plane's own health registry, exposed at GET
	// /metrics (SIGMA-248). Nil is fine and is what handler unit tests pass: the
	// route still exists and still lists every background loop, all of them at
	// "never succeeded", which is the honest reading for a process that is not
	// running them.
	Metrics *cpmetrics.Registry
	// MetricsRetention is how long the sweeper keeps server_metrics rows — the
	// SAME value passed to sweeper.Config.Retention, which is why it is passed
	// in rather than written down here a second time. It caps the window
	// GET .../metrics serves from the fallback store (SIGMA-257); zero means
	// the built-in 24h default. It does not affect the pipeline path, which
	// reads VictoriaMetrics under its own retention.
	MetricsRetention time.Duration
	// OrgAdmin backs the tombstone check on provisioning (SIGMA-298). Nil in
	// handler unit tests that do not exercise it; provisioning then skips the
	// check, which is safe because a test store has no tombstones.
	OrgAdmin OrgAdminAPI
}

// New builds the HTTP surface.
func New(log *slog.Logger, db Pinger, st StoreAPI, dom DomainAPI, opts Options) *Server {
	s := &Server{
		log: log, db: db, store: st, domain: dom,
		git:                 opts.Git,
		inspector:           opts.Inspector,
		repoLister:          opts.RepoLister,
		installTokens:       opts.InstallationTokens,
		installAccounts:     opts.InstallationAccounts,
		gitIntegration:      opts.GitIntegration,
		compose:             opts.Compose,
		clusters:            opts.Clusters,
		registry:            opts.Registry,
		llm:                 opts.LLM,
		models:              opts.Models,
		dns:                 opts.DNS,
		githubAppSlug:       opts.GitHubAppSlug,
		dsdStore:            opts.DSDStore,
		dsdWaiter:           opts.DSDWaiter,
		reconcile:           opts.Reconcile,
		dsdPub:              opts.DSDPublicKey,
		devServiceToken:     opts.DevServiceToken,
		provisionToken:      opts.ProvisionToken,
		githubWebhookSecret: opts.GitHubWebhookSecret,
		publicURL:           strings.TrimRight(opts.PublicURL, "/"),
		dbEngines:           opts.DBEngines,
		s3Engines:           opts.S3Engines,
		telemetry:           opts.Telemetry,
		tel:                 opts.TelemetryStore,
		alertSender:         opts.AlertSender,
		billing:             opts.Billing,
		paddle:              opts.Paddle,
		paddleWebhookSecret: opts.PaddleWebhookSecret,
		paddlePriceID:       opts.PaddlePriceID,
		requireActor:        opts.RequireActor,
		release:             opts.Release.normalized(),
		metrics:             opts.Metrics,
		metricsRetentionCfg: opts.MetricsRetention,
		orgAdmin:            opts.OrgAdmin,
		mux:                 http.NewServeMux(),
	}
	if s.metrics == nil {
		s.metrics = cpmetrics.New()
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	// The control plane's own health (SIGMA-248). Unauthenticated, like the two
	// probes above, and for the same reason: whatever scrapes it runs outside
	// this process and often before any credential exists. It is safe to leave
	// open because it carries no tenant data — the series are loop names,
	// counters, timings and pool gauges, with no org, server or resource
	// identifier in any label. Anything org-scoped belongs on the tenant
	// telemetry routes, which are authenticated.
	s.mux.HandleFunc("GET /metrics", s.metrics.ServeHTTP)

	// Agent onboarding downloads. UNAUTHENTICATED by necessity — the host being
	// onboarded holds no credential yet, and the bootstrap token in the rendered
	// command belongs to the agent, not to a download. They exist so a PRIVATE
	// release repository can be onboarded from: the control plane fetches from
	// GitHub with a server-side credential, so nothing an operator pastes into a
	// terminal carries one. installer.go argues the whole surface — why the
	// asset name is an allowlist rather than a sanitised path, why cosign
	// verification is untouched by a proxy, and why neither route is rate
	// limited or cached here.
	s.mux.HandleFunc("GET /install.sh", s.handleInstallScript)
	s.mux.HandleFunc("GET /dl/{version}/{asset}", s.handleReleaseAsset)

	// Org provisioning (dedicated token; mints the org's web credential).
	s.mux.HandleFunc("POST /v1/orgs", s.requireProvision(s.handleProvisionOrg))
	// …and its inverse: tenant erasure (SIGMA-284), same credential.
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}", s.requireProvision(s.handlePurgeOrg))

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
	// What this control plane can be asked for: the engine sets a deployment
	// has enabled, so the wizard stops offering an engine that 422s at create
	// (SIGMA-268). See capabilities.go.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/capabilities", s.requireService(store.RoleDeveloper, s.handleCapabilities))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/bootstrap-tokens", s.requireService(store.RoleProjectAdmin, s.handleIssueBootstrapToken))
	// SSH onboarding (P1-5): pre-create the server + mint a per-server bootstrap
	// keypair. Project Admin+; like token minting, not idempotency-replayable.
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/servers/provision", s.requireService(store.RoleProjectAdmin, s.handleProvisionServer))
	// Re-issue the install command for a pre-created server that never finished
	// onboarding (lost/expired token). 409 unless it is still `provisioning`.
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/servers/{serverId}/reissue-token", s.requireService(store.RoleProjectAdmin, s.handleReissueBootstrapToken))
	// Dashboard-driven agent upgrade: sets the desired version; the reconciler
	// renders agent.update until the agent converges.
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/servers/{serverId}/agent-update", s.requireService(store.RoleProjectAdmin, s.handleAgentUpdate))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/servers", s.requireService(store.RoleDeveloper, s.handleListServers))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/servers/{serverId}", s.requireService(store.RoleDeveloper, s.handleGetServer))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/servers/{serverId}/metrics", s.requireService(store.RoleDeveloper, s.handleGetMetrics))
	// The two exits from an incompatible enrollment, and the naming the connect
	// form no longer asks for (SIGMA-202/203). `type` re-runs the compatibility
	// gate on the stored facts and answers with the server's new state; the
	// other exit is the DELETE below.
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/servers/{serverId}/type", s.requireService(store.RoleProjectAdmin, s.handleSetServerType))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/servers/{serverId}/rename", s.requireService(store.RoleProjectAdmin, s.handleRenameServer))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/servers/{serverId}/proxy-role", s.requireService(store.RoleProjectAdmin, s.handleProxyRole))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/servers/{serverId}/hardening", s.requireService(store.RoleProjectAdmin, s.handleSetHardening))
	// Server + token lifecycle (P1-4). Server delete and agent-token revoke are
	// Project Admin+; service-token lifecycle is Org Admin only.
	//
	// Disconnect is TWO endpoints since SIGMA-204. POST .../decommission is the
	// ordinary one: it asks the agent to remove the workloads and itself, and
	// the row is tombstoned when the agent acks (or the sweeper times it out).
	// DELETE is the force path — tombstone and revoke, host untouched — for a
	// machine that is already unreachable or a teardown that never finished.
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/servers/{serverId}/decommission", s.requireService(store.RoleProjectAdmin, s.handleDecommissionServer))
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
	s.mux.HandleFunc("PATCH /v1/orgs/{orgId}/environments/{envId}", s.requireService(store.RoleProjectAdmin, s.handleUpdateEnvironment))
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
	// S3 bucket/key/quota CRUD (SIGMA-65): list is member-visible; bucket + key +
	// quota mutations are Project Admin+ and re-render the host server's DSD.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/resources/{resourceId}/buckets", s.requireService(store.RoleDeveloper, s.handleListBuckets))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/resources/{resourceId}/buckets", s.requireService(store.RoleProjectAdmin, s.handleCreateBucket))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/resources/{resourceId}/buckets/{bucket}", s.requireService(store.RoleProjectAdmin, s.handleDeleteBucket))
	s.mux.HandleFunc("PUT /v1/orgs/{orgId}/resources/{resourceId}/buckets/{bucket}/quota", s.requireService(store.RoleProjectAdmin, s.handleSetBucketQuota))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/resources/{resourceId}/buckets/{bucket}/key", s.requireService(store.RoleProjectAdmin, s.handleCreateBucketKey))

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
	// Repo-key custody (SIGMA-170). Exporting hands out the key that decrypts a
	// customer's offsite snapshots, so it is Org Admin and audited; the listing
	// is metadata only but kept at the same level since it enumerates what is
	// exportable.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/backup-repo-keys", s.requireService(store.RoleOrgAdmin, s.handleListArchivedRepoKeys))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/resources/{resourceId}/backup-repo-key/export", s.requireService(store.RoleOrgAdmin, s.handleExportRepoKey))

	// Custom domains (P1-8): attach/detach are Project Admin+, listing is member-visible.
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/resources/{resourceId}/domains", s.requireService(store.RoleProjectAdmin, s.handleAttachDomain))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/resources/{resourceId}/domains", s.requireService(store.RoleDeveloper, s.handleListDomains))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/domains/{domainId}", s.requireService(store.RoleProjectAdmin, s.handleDetachDomain))

	// Deployments (P1-9): release history + build-log stream are member-visible;
	// a rollback (re-ships a prior release) is Project Admin+.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/resources/{resourceId}/deployments", s.requireService(store.RoleDeveloper, s.handleListDeployments))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/deployments", s.requireService(store.RoleDeveloper, s.handleListOrgDeployments))
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
	// Org-level GitHub integration: connect the App once, then SELECT repos per
	// resource instead of connecting each one by hand. Reading the integration
	// and the repo list is member-visible; changing it is Project Admin+.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/git/integration", s.requireService(store.RoleDeveloper, s.handleGetGitIntegration))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/git/integration", s.requireService(store.RoleProjectAdmin, s.handleConnectGitIntegration))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/git/integration/{installationId}", s.requireService(store.RoleProjectAdmin, s.handleDisconnectGitIntegration))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/git/repos", s.requireService(store.RoleDeveloper, s.handleListGitRepos))
	// Compose placement: reading the service graph is member-visible; moving a
	// service between servers changes what runs where, so Project Admin+.
	// Kubernetes clusters: reading is member-visible; building one out of the
	// org's servers changes what runs where, so Project Admin+.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/clusters", s.requireService(store.RoleDeveloper, s.handleListClusters))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/clusters", s.requireService(store.RoleProjectAdmin, s.idempotent(s.handleCreateCluster)))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/clusters/{clusterId}/nodes", s.requireService(store.RoleProjectAdmin, s.handleAddClusterNode))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/clusters/{clusterId}/nodes/{serverId}", s.requireService(store.RoleProjectAdmin, s.handleRemoveClusterNode))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/clusters/{clusterId}", s.requireService(store.RoleProjectAdmin, s.handleDeleteCluster))
	// The org's image registry. Reading it is member-visible (the dashboard shows
	// where images go); writing it is org-admin, because the credential it stores
	// is push access to every image the org builds.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/registry", s.requireService(store.RoleDeveloper, s.handleGetRegistry))
	s.mux.HandleFunc("PUT /v1/orgs/{orgId}/registry", s.requireService(store.RoleOrgAdmin, s.handleSetRegistry))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/registry", s.requireService(store.RoleOrgAdmin, s.handleDeleteRegistry))
	// GPU model hosting: the endpoint readout is member-visible, as are the
	// runtimes this control plane can actually render.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/resources/{resourceId}/llm", s.requireService(store.RoleDeveloper, s.handleGetLLM))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/llm/engines", s.requireService(store.RoleDeveloper, s.handleListLLMEngines))
	// The Hugging Face model picker (SIGMA-213/214). Member-visible like the
	// engine list: browsing the Hub is a read, and every card it returns is
	// public information plus this control plane's own sizing arithmetic. They
	// sit at the same Developer bar so the wizard needs exactly one role to
	// complete its LLM step.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/llm/models", s.requireService(store.RoleDeveloper, s.handleSearchModels))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/llm/models/resolve", s.requireService(store.RoleDeveloper, s.handleResolveModel))
	// DNS setup for a custom domain: which record to create and whether it is
	// live. Member-visible — knowing why a domain doesn't route isn't a mutation.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/domains/{domainId}/dns", s.requireService(store.RoleDeveloper, s.handleDomainDNS))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/resources/{resourceId}/compose", s.requireService(store.RoleDeveloper, s.handleGetComposeServices))
	s.mux.HandleFunc("PUT /v1/orgs/{orgId}/resources/{resourceId}/compose/placements", s.requireService(store.RoleProjectAdmin, s.handleSetComposePlacements))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/git/repos/select", s.requireService(store.RoleProjectAdmin, s.handleSelectGitRepo))
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
	// In-place value rotation (SIGMA-264) — one config deployment, id preserved.
	s.mux.HandleFunc("PUT /v1/orgs/{orgId}/secrets/{secretId}", s.requireService(store.RoleProjectAdmin, s.handleUpdateSecretValue))
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
	// S3 bucket/key/quota ops (SIGMA-65): audited per-op credential release + the
	// agent's terminal status report.
	s.mux.HandleFunc("GET /v1/agent/s3-op-credential", s.requireAgent(s.handleAgentS3OpCredential))
	s.mux.HandleFunc("POST /v1/agent/s3-op-status", s.requireAgent(s.handleAgentS3OpStatus))
	// A cluster node's own account of whether k3s came up on it, and the
	// registry credential a build server needs to push what it built.
	s.mux.HandleFunc("POST /v1/agent/cluster-status", s.requireAgent(s.handleAgentClusterStatus))
	// The agent's final word on a graceful decommission (SIGMA-204). Sent from
	// inside the uninstall handler, BEFORE the teardown reaches the WireGuard
	// interface and the data dir that holds this very credential — which is why
	// it cannot be the ordinary DSD op-status report.
	s.mux.HandleFunc("POST /v1/agent/uninstall-ack", s.requireAgent(s.handleAgentUninstallAck))
	s.mux.HandleFunc("GET /v1/agent/registry-credential", s.requireAgent(s.handleAgentRegistryCredential))
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

// Unwrap lets http.NewResponseController reach the underlying writer, and Flush
// forwards to it when it supports flushing. Without these, wrapping the mux in
// withLogging hid the http.Flusher of the real writer (interface embedding
// promotes only ResponseWriter's methods), so the deploy-log SSE handler's
// `w.(http.Flusher)` assertion always failed and returned 500 (SIGMA-133).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
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
