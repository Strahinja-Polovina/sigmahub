package api

// Kubernetes cluster endpoints: build a cluster from the org's own servers,
// add/remove worker nodes, and read the cluster's state.

import (
	"context"
	"errors"
	"net/http"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// ClusterAPI is the cluster slice of the store.
type ClusterAPI interface {
	CreateCluster(ctx context.Context, orgID string, in store.CreateClusterInput, actor string) (store.Cluster, error)
	ListClusters(ctx context.Context, orgID, environmentID string) ([]store.Cluster, error)
	AddClusterNode(ctx context.Context, orgID, clusterID, serverID, actor string) error
	RemoveClusterNode(ctx context.Context, orgID, clusterID, serverID, actor string) error
	DeleteCluster(ctx context.Context, orgID, clusterID, actor string) ([]string, error)
	// ReportClusterNode records what a node says about k3s on it and rederives
	// the cluster's status from the node rows.
	ReportClusterNode(ctx context.Context, serverID string, rep store.ClusterNodeReport) (clusterID, status string, err error)
}

// handleAgentClusterStatus is how a cluster stops reading "provisioning"
// forever. The node is the only thing that knows whether k3s actually came up
// on it, so it reports — and the report is scoped to the reporting server by the
// agent token: a node can say something about itself and nothing else.
func (s *Server) handleAgentClusterStatus(w http.ResponseWriter, r *http.Request) {
	if s.clusters == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "clusters are not configured"})
		return
	}
	srv := serverFrom(r)
	var req struct {
		Ready       bool   `json:"ready"`
		Message     string `json:"message"`
		APIEndpoint string `json:"apiEndpoint"`
		Version     string `json:"version"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	clusterID, status, err := s.clusters.ReportClusterNode(r.Context(), srv.ID, store.ClusterNodeReport{
		Ready:       req.Ready,
		Message:     req.Message,
		APIEndpoint: req.APIEndpoint,
		Version:     req.Version,
	})
	if errors.Is(err, store.ErrNotFound) {
		// The server left the cluster between rendering the op and reporting on
		// it. Accept and discard rather than making the agent retry a report
		// about a membership that no longer exists.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	if err != nil {
		s.writeStoreErr(w, err, "cluster status")
		return
	}
	// A ready control plane is what lets workloads render at all, so the node's
	// own document has to be rebuilt as soon as it says so.
	if s.reconcile != nil {
		s.reconcile.ReconcileAsync(srv.OrgID, srv.ID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status, "clusterId": clusterID})
}

func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	if s.clusters == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "clusters are not configured"})
		return
	}
	list, err := s.clusters.ListClusters(r.Context(), r.PathValue("orgId"), r.URL.Query().Get("environmentId"))
	if err != nil {
		s.writeStoreErr(w, err, "list clusters")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"clusters": list,
		// Published so the dashboard explains the rule instead of keeping its
		// own copy of which kinds a cluster refuses.
		"excludedKinds": store.ClusterExcludedKinds(),
	})
}

func (s *Server) handleCreateCluster(w http.ResponseWriter, r *http.Request) {
	if s.clusters == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "clusters are not configured"})
		return
	}
	var req struct {
		EnvironmentID  string `json:"environmentId"`
		Name           string `json:"name"`
		ControlPlaneID string `json:"controlPlaneId"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	orgID := r.PathValue("orgId")
	c, err := s.clusters.CreateCluster(r.Context(), orgID, store.CreateClusterInput{
		EnvironmentID:  req.EnvironmentID,
		Name:           req.Name,
		ControlPlaneID: req.ControlPlaneID,
	}, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "create cluster")
		return
	}
	// The control-plane node's document gains the k8s.node op.
	if s.reconcile != nil {
		s.reconcile.ReconcileAsync(orgID, req.ControlPlaneID)
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleAddClusterNode(w http.ResponseWriter, r *http.Request) {
	if s.clusters == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "clusters are not configured"})
		return
	}
	var req struct {
		ServerID string `json:"serverId"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	orgID, clusterID := r.PathValue("orgId"), r.PathValue("clusterId")
	if err := s.clusters.AddClusterNode(r.Context(), orgID, clusterID, req.ServerID, principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "add cluster node")
		return
	}
	if s.reconcile != nil {
		s.reconcile.ReconcileAsync(orgID, req.ServerID)
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "joined"})
}

func (s *Server) handleRemoveClusterNode(w http.ResponseWriter, r *http.Request) {
	if s.clusters == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "clusters are not configured"})
		return
	}
	orgID, clusterID := r.PathValue("orgId"), r.PathValue("clusterId")
	serverID := r.PathValue("serverId")
	err := s.clusters.RemoveClusterNode(r.Context(), orgID, clusterID, serverID, principalFrom(r).Name)
	if errors.Is(err, store.ErrControlPlaneNode) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "the control-plane node runs the API server; delete the cluster instead of removing it",
		})
		return
	}
	if err != nil {
		s.writeStoreErr(w, err, "remove cluster node")
		return
	}
	// The removed server's document must stop describing the cluster so k3s is
	// torn down there rather than left running unmanaged.
	if s.reconcile != nil {
		s.reconcile.ReconcileAsync(orgID, serverID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func (s *Server) handleDeleteCluster(w http.ResponseWriter, r *http.Request) {
	if s.clusters == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "clusters are not configured"})
		return
	}
	orgID := r.PathValue("orgId")
	servers, err := s.clusters.DeleteCluster(r.Context(), orgID, r.PathValue("clusterId"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "delete cluster")
		return
	}
	if s.reconcile != nil {
		for _, id := range servers {
			s.reconcile.ReconcileAsync(orgID, id)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "servers": servers})
}

// LLMAPI is the model-hosting slice of the store.
type LLMAPI interface {
	GetLLM(ctx context.Context, orgID, resourceID string) (store.LLMInfo, error)
}

// DNSAPI derives and verifies a custom domain's DNS records.
type DNSAPI interface {
	DNSSetupForDomain(ctx context.Context, orgID, domainID string) (store.DNSSetup, error)
}
