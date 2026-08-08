package reconciler

// Kubernetes (k3s) rendering.
//
// Two ops carry the whole model. k8s.node brings a server up as a cluster
// member — the control plane runs the API server and mints nothing else, a
// worker joins it over the mesh with the cluster token. k8s.apply reconciles one
// workload through the control-plane node.
//
// Workloads render ONLY into the control-plane node's document: kubectl talks to
// the API server and the scheduler decides which node actually runs a pod, so
// rendering a workload per node would create N competing appliers of the same
// Deployment.

import (
	"encoding/json"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// k8sNodeOpSpec mirrors the agent's k8s.NodeSpec.
type k8sNodeOpSpec struct {
	ClusterID string `json:"clusterId"`
	Name      string `json:"name"`
	Role      string `json:"role"` // control-plane|worker
	// JoinToken authenticates the node into the cluster. The control plane sets
	// it as the cluster secret; a worker presents it.
	JoinToken string `json:"joinToken"`
	// ServerURL is what a worker dials (https://<control-plane mesh ip>:6443).
	// Empty for the control-plane node itself.
	ServerURL string `json:"serverUrl,omitempty"`
	// AdvertiseIP binds the API server to the mesh address, so the cluster is
	// reachable org-mesh-wide and nowhere else — the same invariant databases
	// and object storage already hold to.
	AdvertiseIP string `json:"advertiseIp"`
}

// k8sApplyOpSpec mirrors the agent's k8s.ApplySpec — one workload.
type k8sApplyOpSpec struct {
	ResourceID string            `json:"resourceId"`
	Name       string            `json:"name"`
	Namespace  string            `json:"namespace"`
	Image      string            `json:"image"`
	Replicas   int               `json:"replicas"`
	Ports      []int             `json:"ports,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	SecretRefs []secretRef       `json:"secretRefs,omitempty"`
	// Hosts are the domains routed to this workload by the cluster ingress.
	Hosts []string `json:"hosts,omitempty"`
	// DeploymentID lets the agent report per-deployment status.
	DeploymentID string `json:"deploymentId,omitempty"`
}

// defaultClusterReplicas is what a workload gets when nothing asked otherwise.
// One replica keeps a cluster deploy behaving like a server deploy until the
// user opts into more, rather than silently tripling their resource usage.
const defaultClusterReplicas = 1

// renderClusterNodeOp emits the membership op for a server in a cluster.
// ok=false before the mesh is up: k3s binds to the mesh address, so joining
// without one would publish the API server on an undefined interface.
func renderClusterNodeOp(m store.ClusterMembership, meshIP string) (dsd.Op, bool) {
	if meshIP == "" || m.JoinToken == "" {
		return dsd.Op{}, false
	}
	spec := k8sNodeOpSpec{
		ClusterID:   m.ClusterID,
		Name:        m.Name,
		Role:        m.Role,
		JoinToken:   m.JoinToken,
		AdvertiseIP: meshIP,
	}
	if m.Role != store.NodeRoleControlPlane {
		// A worker needs the control plane's address. Until that node has a mesh
		// IP there is nothing to dial, so hold the join rather than emitting an
		// op that can only fail.
		if m.ControlPlaneMeshIP == "" {
			return dsd.Op{}, false
		}
		spec.ServerURL = "https://" + m.ControlPlaneMeshIP + ":6443"
	}
	raw, _ := json.Marshal(spec)
	return dsd.Op{ID: "k8s:node:" + m.ClusterID, Kind: dsd.KindK8sNode, Spec: raw}, true
}

// renderClusterWorkloadOps expands a cluster-deployed app into its k8s.apply op.
// Rendered only for the control-plane node — see the package comment.
func renderClusterWorkloadOps(rs store.ResourceSpec, refs []store.SecretRefMeta, domains []store.Domain, target store.DeployTarget, m store.ClusterMembership, nodeOpID string) (ops []dsd.Op, ok bool) {
	if m.Role != store.NodeRoleControlPlane {
		return nil, false
	}
	var spec appResourceSpec
	if err := json.Unmarshal(rs.Spec, &spec); err != nil {
		return nil, false
	}

	// The image the workload runs. A git-deployed app runs its per-deployment
	// pinned tag (the same pin the server path uses, so a rollback re-ships the
	// exact bytes); a registry-image app runs its declared image.
	image := spec.Image
	deploymentID := ""
	if target.DeploymentID != "" {
		image = dsd.PinnedImageTag(rs.ResourceID, target.SHA, target.ImagePin)
		deploymentID = target.DeploymentID
	}
	if image == "" {
		return nil, false
	}

	apply := k8sApplyOpSpec{
		ResourceID:   rs.ResourceID,
		Name:         dsd.ContainerName(rs.ResourceID),
		Namespace:    dsd.NetworkName(rs.ProjectID),
		Image:        image,
		Replicas:     defaultClusterReplicas,
		Env:          spec.Env,
		DeploymentID: deploymentID,
	}
	for _, p := range spec.Ports {
		if p.Container > 0 {
			apply.Ports = append(apply.Ports, p.Container)
		}
	}
	for _, r := range refs {
		apply.SecretRefs = append(apply.SecretRefs, secretRef{Name: r.Name, EnvVar: r.EnvVar})
	}
	for _, d := range domains {
		apply.Hosts = append(apply.Hosts, d.Domain)
	}

	raw, _ := json.Marshal(apply)
	// The node must be up before any workload is applied.
	deps := []string{}
	if nodeOpID != "" {
		deps = append(deps, nodeOpID)
	}
	return []dsd.Op{{
		ID:        "res:" + rs.ResourceID,
		Kind:      dsd.KindK8sApply,
		DependsOn: deps,
		Spec:      raw,
	}}, true
}
