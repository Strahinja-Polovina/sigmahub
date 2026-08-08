package store

// Kubernetes clusters (k3s) built from the org's own servers.
//
// A cluster is scoped to an environment, so "deploy to the cluster in staging"
// is unambiguous. Exactly one node is the control plane in v1; the rest are
// workers. Databases are deliberately excluded — see ClusterKindAllowed.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Cluster is a k3s cluster over the org's servers.
type Cluster struct {
	ID            string        `json:"id"`
	OrgID         string        `json:"orgId"`
	EnvironmentID string        `json:"environmentId"`
	Name          string        `json:"name"`
	Status        string        `json:"status"` // provisioning|ready|degraded
	APIEndpoint   string        `json:"apiEndpoint"`
	Version       string        `json:"kubernetesVersion"`
	CreatedBy     string        `json:"createdBy"`
	CreatedAt     time.Time     `json:"createdAt"`
	Nodes         []ClusterNode `json:"nodes"`
}

// ClusterNode is one server's membership in a cluster.
type ClusterNode struct {
	ServerID   string    `json:"serverId"`
	ServerName string    `json:"serverName"`
	ServerType string    `json:"serverType"`
	Status     string    `json:"status"` // the server's own status
	MeshIP     string    `json:"meshIp"`
	Role       string    `json:"role"` // control-plane|worker
	JoinedAt   time.Time `json:"joinedAt"`
	// NodeStatus is what the node itself last reported about k3s on it:
	// pending|ready|error. Distinct from Status, which is only whether the
	// AGENT is checking in — an agent can be perfectly healthy on a host where
	// the k3s install failed, and reading one as the other is how a cluster ends
	// up looking fine while nothing can be scheduled on it.
	NodeStatus  string     `json:"nodeStatus"`
	NodeMessage string     `json:"nodeMessage,omitempty"`
	ReportedAt  *time.Time `json:"reportedAt,omitempty"`
}

// Cluster node roles.
const (
	NodeRoleControlPlane = "control-plane"
	NodeRoleWorker       = "worker"
)

// clusterExcludedKinds are resource kinds that must NOT run inside a cluster.
//
// Every one of them is a stateful engine whose data lives in a volume on one
// host. In a scheduler that means node affinity, PV lifecycle and eviction
// semantics we do not model — and a database silently rescheduled onto a node
// without its data is data loss, not a degraded deploy. Managed databases and
// object storage stay on their own server; the cluster reaches them over the
// mesh exactly like anything else.
var clusterExcludedKinds = map[string]bool{
	"postgres": true,
	"mysql":    true,
	"redis":    true,
	"mongodb":  true,
	"s3":       true,
}

// ClusterKindAllowed reports whether a resource kind may be deployed INTO a
// cluster.
func ClusterKindAllowed(kind string) bool { return !clusterExcludedKinds[kind] }

// ClusterExcludedKinds lists the kinds a cluster refuses, for the API to
// publish so the dashboard explains the rule instead of hardcoding its own copy.
func ClusterExcludedKinds() []string {
	out := make([]string, 0, len(clusterExcludedKinds))
	for k := range clusterExcludedKinds {
		out = append(out, k)
	}
	return out
}

// ErrKindNotClusterable is returned when a stateful kind is aimed at a cluster.
type ErrKindNotClusterable struct{ Kind string }

func (e ErrKindNotClusterable) Error() string {
	return fmt.Sprintf("%s runs on its own server, not inside a cluster", e.Kind)
}

// clusterTokenPurpose namespaces the join-token wrapping key per org.
func clusterTokenPurpose(orgID string) string { return "cluster_token:" + orgID }

// CreateClusterInput creates a cluster and promotes its control-plane node.
type CreateClusterInput struct {
	EnvironmentID  string
	Name           string
	ControlPlaneID string // server that becomes the control plane
}

// CreateCluster promotes a server into a cluster's control plane and mints the
// join token workers authenticate with. The token is KMS-wrapped immediately —
// anyone holding it can join a node to the cluster.
func (s *Store) CreateCluster(ctx context.Context, orgID string, in CreateClusterInput, actor string) (Cluster, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || strings.TrimSpace(in.EnvironmentID) == "" || strings.TrimSpace(in.ControlPlaneID) == "" {
		return Cluster{}, ErrInvalid{Msg: "name, environmentId and controlPlaneId are required"}
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Cluster{}, fmt.Errorf("generate cluster token: %w", err)
	}
	token := hex.EncodeToString(raw)
	wrapped, err := s.custody.Wrap(ctx, clusterTokenPurpose(orgID), []byte(token))
	if err != nil {
		return Cluster{}, fmt.Errorf("wrap cluster token: %w", err)
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Cluster{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The environment must belong to the org (tenant isolation).
	var envOK bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM environments e JOIN projects p ON p.id = e.project_id
			 WHERE e.id = $1 AND p.org_id = $2)`, in.EnvironmentID, orgID).Scan(&envOK); err != nil {
		return Cluster{}, err
	}
	if !envOK {
		return Cluster{}, ErrNotFound
	}
	if err := assertServerInOrg(ctx, tx, orgID, in.ControlPlaneID); err != nil {
		return Cluster{}, err
	}

	c := Cluster{
		ID: newID("cls"), OrgID: orgID, EnvironmentID: in.EnvironmentID,
		Name: name, Status: "provisioning", CreatedBy: actor,
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO clusters (id, org_id, environment_id, name, status, join_token_wrapped, created_by)
		VALUES ($1,$2,$3,$4,'provisioning',$5,$6)
		RETURNING created_at`,
		c.ID, c.OrgID, c.EnvironmentID, c.Name, wrapped, actor).Scan(&c.CreatedAt)
	if isUniqueViolation(err) {
		return Cluster{}, fmt.Errorf("%w: this environment already has a cluster", ErrConflict)
	}
	if err != nil {
		return Cluster{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO cluster_nodes (cluster_id, server_id, role) VALUES ($1,$2,$3)`,
		c.ID, in.ControlPlaneID, NodeRoleControlPlane); err != nil {
		if isUniqueViolation(err) {
			return Cluster{}, fmt.Errorf("%w: that server already belongs to a cluster", ErrConflict)
		}
		return Cluster{}, err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Cluster created", name); err != nil {
		return Cluster{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Cluster{}, err
	}
	return c, nil
}

func assertServerInOrg(ctx context.Context, tx pgx.Tx, orgID, serverID string) error {
	var ok bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM servers WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL)`,
		orgID, serverID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// AddClusterNode joins a server to a cluster as a worker.
func (s *Store) AddClusterNode(ctx context.Context, orgID, clusterID, serverID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := assertClusterInOrg(ctx, tx, orgID, clusterID); err != nil {
		return err
	}
	if err := assertServerInOrg(ctx, tx, orgID, serverID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO cluster_nodes (cluster_id, server_id, role) VALUES ($1,$2,$3)
		ON CONFLICT (cluster_id, server_id) DO NOTHING`, clusterID, serverID, NodeRoleWorker)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: that server already belongs to a cluster", ErrConflict)
	}
	if err != nil {
		return err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Cluster node added", serverID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ErrControlPlaneNode refuses removal of the node running the API server.
var ErrControlPlaneNode = errors.New("the control-plane node cannot be removed; delete the cluster instead")

// RemoveClusterNode drains a worker out of the cluster. The control-plane node
// is refused: removing it destroys the cluster, which must be an explicit
// delete rather than a side effect of a node edit.
func (s *Store) RemoveClusterNode(ctx context.Context, orgID, clusterID, serverID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := assertClusterInOrg(ctx, tx, orgID, clusterID); err != nil {
		return err
	}
	var role string
	err = tx.QueryRow(ctx, `
		SELECT role FROM cluster_nodes WHERE cluster_id = $1 AND server_id = $2 FOR UPDATE`,
		clusterID, serverID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if role == NodeRoleControlPlane {
		return ErrControlPlaneNode
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM cluster_nodes WHERE cluster_id = $1 AND server_id = $2`, clusterID, serverID); err != nil {
		return err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Cluster node removed", serverID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func assertClusterInOrg(ctx context.Context, tx pgx.Tx, orgID, clusterID string) error {
	var ok bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM clusters WHERE org_id = $1 AND id = $2)`,
		orgID, clusterID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// ControlPlaneServerForCluster returns the server running the cluster's API
// server.
//
// It is the only node a workload renders into (see reconciler/k8s.go: the
// scheduler picks the pod's host, so rendering the same Deployment per node
// would create N competing appliers). A cluster resource carries no server_id,
// so a mutation that lands one has nothing to point a re-render at — without
// this the workload first appeared at the next 60-second fleet resync, which
// looks exactly like a create that silently did nothing.
func (s *Store) ControlPlaneServerForCluster(ctx context.Context, orgID, clusterID string) (string, error) {
	var serverID string
	err := s.Pool.QueryRow(ctx, `
		SELECT n.server_id
		  FROM cluster_nodes n JOIN clusters c ON c.id = n.cluster_id
		 WHERE c.org_id = $1 AND c.id = $2 AND n.role = $3`,
		orgID, clusterID, NodeRoleControlPlane).Scan(&serverID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return serverID, err
}

// ListClusters returns the org's clusters with their nodes.
func (s *Store) ListClusters(ctx context.Context, orgID, environmentID string) ([]Cluster, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, org_id, environment_id, name, status, api_endpoint, kubernetes_version, created_by, created_at
		  FROM clusters
		 WHERE org_id = $1 AND ($2 = '' OR environment_id = $2)
		 ORDER BY created_at DESC`, orgID, environmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Cluster{}
	index := map[string]int{}
	for rows.Next() {
		var c Cluster
		if err := rows.Scan(&c.ID, &c.OrgID, &c.EnvironmentID, &c.Name, &c.Status,
			&c.APIEndpoint, &c.Version, &c.CreatedBy, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Nodes = []ClusterNode{}
		index[c.ID] = len(out)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	ids := make([]string, 0, len(out))
	for _, c := range out {
		ids = append(ids, c.ID)
	}
	nodeRows, err := s.Pool.Query(ctx, `
		SELECT n.cluster_id, n.server_id, s.name, s.type, s.status, COALESCE(s.mesh_ip,''), n.role, n.joined_at,
		       n.node_status, n.node_message, n.reported_at
		  FROM cluster_nodes n JOIN servers s ON s.id = n.server_id
		 WHERE n.cluster_id = ANY($1)
		 ORDER BY n.role, s.name`, ids)
	if err != nil {
		return nil, err
	}
	defer nodeRows.Close()
	for nodeRows.Next() {
		var clusterID string
		var n ClusterNode
		if err := nodeRows.Scan(&clusterID, &n.ServerID, &n.ServerName, &n.ServerType,
			&n.Status, &n.MeshIP, &n.Role, &n.JoinedAt,
			&n.NodeStatus, &n.NodeMessage, &n.ReportedAt); err != nil {
			return nil, err
		}
		if i, ok := index[clusterID]; ok {
			out[i].Nodes = append(out[i].Nodes, n)
		}
	}
	return out, nodeRows.Err()
}

// DeleteCluster removes a cluster. Resources deployed into it lose their
// binding (ON DELETE SET NULL) rather than being deleted — the workloads are
// the customer's, and a cluster teardown must not take them with it.
func (s *Store) DeleteCluster(ctx context.Context, orgID, clusterID, actor string) ([]string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := assertClusterInOrg(ctx, tx, orgID, clusterID); err != nil {
		return nil, err
	}
	// The member servers need re-rendering so k3s is torn down on each.
	rows, err := tx.Query(ctx, `SELECT server_id FROM cluster_nodes WHERE cluster_id = $1`, clusterID)
	if err != nil {
		return nil, err
	}
	var servers []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		servers = append(servers, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM clusters WHERE id = $1`, clusterID); err != nil {
		return nil, err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Cluster deleted", clusterID); err != nil {
		return nil, err
	}
	return servers, tx.Commit(ctx)
}

// ClusterMembership is a server's role in its cluster, for the reconciler.
type ClusterMembership struct {
	ClusterID   string
	Name        string
	Role        string
	JoinToken   string
	APIEndpoint string
	// ControlPlaneMeshIP is where a worker dials to join.
	ControlPlaneMeshIP string
}

// ClusterMembershipForServer returns the server's cluster role, unwrapping the
// join token for the DSD render. ok=false when the server is in no cluster.
func (s *Store) ClusterMembershipForServer(ctx context.Context, serverID string) (ClusterMembership, bool, error) {
	var m ClusterMembership
	var wrapped []byte
	var orgID string
	err := s.Pool.QueryRow(ctx, `
		SELECT c.id, c.org_id, c.name, n.role, c.join_token_wrapped, c.api_endpoint,
		       COALESCE((SELECT s2.mesh_ip FROM cluster_nodes n2
		                   JOIN servers s2 ON s2.id = n2.server_id
		                  WHERE n2.cluster_id = c.id AND n2.role = 'control-plane'), '')
		  FROM cluster_nodes n JOIN clusters c ON c.id = n.cluster_id
		 WHERE n.server_id = $1`, serverID).
		Scan(&m.ClusterID, &orgID, &m.Name, &m.Role, &wrapped, &m.APIEndpoint, &m.ControlPlaneMeshIP)
	if errors.Is(err, pgx.ErrNoRows) {
		return ClusterMembership{}, false, nil
	}
	if err != nil {
		return ClusterMembership{}, false, err
	}
	if len(wrapped) > 0 {
		plain, uerr := s.custody.Unwrap(ctx, clusterTokenPurpose(orgID), wrapped)
		if uerr != nil {
			return ClusterMembership{}, false, fmt.Errorf("unwrap cluster token: %w", uerr)
		}
		m.JoinToken = string(plain)
	}
	return m, true, nil
}

// Node-report states. A node is 'pending' from the moment it is told to join
// until it says otherwise, so a cluster nobody has provisioned reads as
// provisioning rather than as ready.
const (
	NodeStatusPending = "pending"
	NodeStatusReady   = "ready"
	NodeStatusError   = "error"
)

// ClusterNodeReport is what one node says about k3s on it. Only the control
// plane fills APIEndpoint/Version — a worker has no API server to describe.
type ClusterNodeReport struct {
	Ready       bool
	Message     string
	APIEndpoint string
	Version     string
}

// ReportClusterNode records a node's own account of its k3s state and rederives
// the cluster's status from the node rows.
//
// Before this, `clusters.status` was written once as 'provisioning' and never
// moved: the only thing that could have advanced it had no caller at all, so a
// cluster that came up perfectly and one whose control plane never installed
// were indistinguishable in the product. Deriving the cluster status here — in
// the same transaction as the node row — means the two can never disagree.
//
// Scoped by serverID, which the caller has already authenticated from the agent
// token: a node can only ever report about itself.
func (s *Store) ReportClusterNode(ctx context.Context, serverID string, rep ClusterNodeReport) (clusterID, status string, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var role string
	err = tx.QueryRow(ctx, `
		SELECT n.cluster_id, n.role FROM cluster_nodes n WHERE n.server_id = $1 FOR UPDATE`,
		serverID).Scan(&clusterID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		// The server left the cluster between rendering the op and reporting on
		// it. Not an error: the node is being torn down and its report is stale.
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}

	nodeStatus := NodeStatusError
	if rep.Ready {
		nodeStatus = NodeStatusReady
	}
	if _, err := tx.Exec(ctx, `
		UPDATE cluster_nodes SET node_status = $3, node_message = $4, reported_at = now()
		 WHERE cluster_id = $1 AND server_id = $2`,
		clusterID, serverID, nodeStatus, strings.TrimSpace(rep.Message)); err != nil {
		return "", "", err
	}

	// The control plane is the only node that can describe the API server, and
	// only while it is actually up — a stale endpoint on a broken control plane
	// is worse than none, because it is what kubectl would be told to dial.
	if role == NodeRoleControlPlane && rep.Ready {
		if _, err := tx.Exec(ctx, `
			UPDATE clusters
			   SET api_endpoint = COALESCE(NULLIF($2,''), api_endpoint),
			       kubernetes_version = COALESCE(NULLIF($3,''), kubernetes_version)
			 WHERE id = $1`, clusterID, strings.TrimSpace(rep.APIEndpoint), strings.TrimSpace(rep.Version)); err != nil {
			return "", "", err
		}
	}

	status, err = rederiveClusterStatusTx(ctx, tx, clusterID)
	if err != nil {
		return "", "", err
	}
	return clusterID, status, tx.Commit(ctx)
}

// rederiveClusterStatusTx computes a cluster's status from its node rows.
//
//	ready       — the control plane is up and every node reported ready
//	degraded    — the control plane is up but some node did not
//	provisioning— the control plane has not reported ready yet
//
// Nothing else writes clusters.status, so the derivation is the definition.
func rederiveClusterStatusTx(ctx context.Context, tx pgx.Tx, clusterID string) (string, error) {
	var cpReady bool
	var notReady int
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(bool_or(role = 'control-plane' AND node_status = 'ready'), false),
		       COUNT(*) FILTER (WHERE node_status <> 'ready')
		  FROM cluster_nodes WHERE cluster_id = $1`, clusterID).Scan(&cpReady, &notReady); err != nil {
		return "", err
	}
	status := "provisioning"
	switch {
	case cpReady && notReady == 0:
		status = "ready"
	case cpReady:
		status = "degraded"
	}
	if _, err := tx.Exec(ctx,
		`UPDATE clusters SET status = $2, updated_at = now() WHERE id = $1`, clusterID, status); err != nil {
		return "", err
	}
	return status, nil
}

// ClusterBuildSpecsForServer returns cluster-deployed resources whose current
// deployment names this server as the build server.
//
// A cluster workload has no server of its own, so it never appears in
// ResourceSpecsForServer and its clone+build ops landed in no document at all —
// the manifest pointed at an image tag nothing had ever built. The build has to
// happen SOMEWHERE, and a build server is the only place the product models.
func (s *Store) ClusterBuildSpecsForServer(ctx context.Context, serverID string) ([]ResourceSpec, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT r.id, r.project_id, r.kind, r.spec, r.ephemeral
		  FROM resources r
		  JOIN deployments d ON d.resource_id = r.id
		 WHERE r.cluster_id IS NOT NULL
		   AND d.build_server_id = $1
		   AND d.status IN ('queued','building','deploying')
		 ORDER BY r.id`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResourceSpec{}
	for rows.Next() {
		var r ResourceSpec
		if err := rows.Scan(&r.ResourceID, &r.ProjectID, &r.Kind, &r.Spec, &r.Ephemeral); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResourceSpecsForCluster returns the resources deployed INTO a cluster. They
// render into the control-plane node's document, not any single server's.
func (s *Store) ResourceSpecsForCluster(ctx context.Context, clusterID string) ([]ResourceSpec, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, project_id, kind, spec, ephemeral FROM resources
		 WHERE cluster_id = $1 ORDER BY created_at`, clusterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResourceSpec{}
	for rows.Next() {
		var r ResourceSpec
		if err := rows.Scan(&r.ResourceID, &r.ProjectID, &r.Kind, &r.Spec, &r.Ephemeral); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeployTargetForResource returns the current deploy target for one resource,
// regardless of which server (or cluster) it lands on. The per-server query
// cannot serve cluster workloads: they have no server_id of their own.
func (s *Store) DeployTargetForResource(ctx context.Context, resourceID string) (DeployTarget, error) {
	var t DeployTarget
	var ref, sha, cfg, digest *string
	var svcStatus []byte
	err := s.Pool.QueryRow(ctx, `
		SELECT d.id, d.resource_id, r.project_id, d.connection_id, c.provider, c.repo_full_name,
		       d.git_ref, d.git_sha, d.config_hash, d.image_digest, COALESCE(d.image_pin,''), d.trigger, d.status,
		       d.created_at, d.service_status, COALESCE(d.server_id, ''), COALESCE(d.build_server_id, '')
		  FROM deployments d
		  JOIN resources r ON r.id = d.resource_id
		  JOIN git_connections c ON c.id = d.connection_id
		 WHERE d.resource_id = $1 AND d.status IN ('queued','building','deploying','success')
		 ORDER BY d.created_at DESC LIMIT 1`, resourceID).
		Scan(&t.DeploymentID, &t.ResourceID, &t.ProjectID, &t.ConnectionID, &t.Provider,
			&t.RepoFullName, &ref, &sha, &cfg, &digest, &t.ImagePin, &t.Trigger, &t.Status, &t.CreatedAt,
			&svcStatus, &t.ServerID, &t.BuildServerID)
	if err != nil {
		return DeployTarget{}, err
	}
	t.Ref, t.SHA, t.ConfigHash, t.ImageDigest = derefOr(ref), derefOr(sha), derefOr(cfg), derefOr(digest)
	if len(svcStatus) > 0 {
		_ = json.Unmarshal(svcStatus, &t.ServiceStatus)
	}
	return t, nil
}

func derefOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
