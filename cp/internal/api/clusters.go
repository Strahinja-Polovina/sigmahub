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
