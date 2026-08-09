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
//
// Images are the thing a cluster changes about a deploy. On a server the build
// and the run share one Docker daemon, so a local tag is enough; in a cluster
// the scheduler picks the node, so the image has to exist somewhere every node
// can pull from. That means a registry and a build server — and when either is
// missing the right answer is to render nothing and say why, not to apply a
// manifest that can only ever be ImagePullBackOff.

import (
	"encoding/json"
	"sort"

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
	ResourceID string `json:"resourceId"`
	// Service is the Compose service this workload came from, empty for a
	// single-container app. It makes each service its own Deployment, so a
	// Compose graph deploys as a graph instead of as one opaque pod.
	Service    string            `json:"service,omitempty"`
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
	// RegistryHost, when set, makes the node authenticate its image pull: the
	// agent fetches the credential over its own channel and renders an
	// imagePullSecret. The DSD never carries the password.
	RegistryHost string `json:"registryHost,omitempty"`
	// Workloads is every workload name this resource currently renders. The
	// agent prunes its manifests down to this set, so a service deleted from the
	// Compose file stops running instead of outliving its own definition.
	Workloads []string `json:"workloads,omitempty"`
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

// clusterImageReady reports whether the images a cluster deploy needs can
// actually be pulled by the nodes, and holds the render until they can.
//
//	render=false, gated=true  — a build is in flight; come back on the next pass.
//	render=false, gated=false — nothing will ever produce these images.
//
// The distinction matters: the first is patience, the second is a deploy that
// would hang forever, and the caller surfaces them differently.
func clusterImageReady(target store.DeployTarget, repository string, reship bool) (render, gated bool) {
	if target.DeploymentID == "" {
		return true, false // a registry-image app: the image already exists
	}
	if reship {
		return true, false // rollback/config: the release's images are already pushed
	}
	// A source build has to happen on a machine and land in a registry every
	// node can reach. Neither is optional here.
	if repository == "" || target.BuildServerID == "" {
		return false, false
	}
	// The build lives in the build server's document; ops there are invisible to
	// this one's dependency graph, so gate on the deployment instead — exactly
	// as a cross-server Compose dependency is gated.
	if target.Status == "queued" || target.Status == "building" {
		return false, true
	}
	return true, false
}

// renderClusterWorkloadOps expands a cluster-deployed app into its k8s.apply
// ops: one per Compose service, or a single workload for a plain app. Rendered
// only for the control-plane node — see the package comment.
func renderClusterWorkloadOps(rs store.ResourceSpec, refs []store.SecretRefMeta, domains []store.Domain, target store.DeployTarget, m store.ClusterMembership, nodeOpID, repository, registryHost string) (ops []dsd.Op, ok bool) {
	if m.Role != store.NodeRoleControlPlane {
		return nil, false
	}
	var spec appResourceSpec
	if err := json.Unmarshal(rs.Spec, &spec); err != nil {
		return nil, false
	}
	namespace := dsd.K8sNamespace(rs.ProjectID)
	if namespace == "" {
		return nil, false
	}

	var refsSpec []secretRef
	for _, r := range refs {
		refsSpec = append(refsSpec, secretRef{Name: r.Name, EnvVar: r.EnvVar})
	}
	var hosts []string
	for _, d := range domains {
		hosts = append(hosts, d.Domain)
	}
	// The node must be up before any workload is applied.
	var deps []string
	if nodeOpID != "" {
		deps = append(deps, nodeOpID)
	}

	if spec.Compose != nil && len(spec.Compose.Services) > 0 {
		return renderClusterComposeOps(rs, spec, refsSpec, hosts, target, namespace, deps, repository, registryHost)
	}

	if render, _ := clusterImageReady(target, repository, reshipsRetainedImage(target, false)); !render {
		return nil, false
	}
	name := dsd.K8sWorkloadName(rs.ResourceID, "")
	if name == "" {
		return nil, false
	}

	// The image the workload runs. A git-deployed app runs its per-deployment
	// pinned tag (the same pin the server path uses, so a rollback re-ships the
	// exact bytes); a registry-image app runs its declared image.
	image := spec.Image
	deploymentID := ""
	if target.DeploymentID != "" {
		image = dsd.QualifyImage(repository, dsd.PinnedImageTag(rs.ResourceID, target.SHA, target.ImagePin))
		deploymentID = target.DeploymentID
	}
	if image == "" {
		return nil, false
	}

	apply := k8sApplyOpSpec{
		ResourceID:   rs.ResourceID,
		Name:         name,
		Namespace:    namespace,
		Image:        image,
		Replicas:     defaultClusterReplicas,
		Env:          spec.Env,
		SecretRefs:   refsSpec,
		Hosts:        hosts,
		DeploymentID: deploymentID,
		Workloads:    []string{name},
	}
	// Only a tag we pushed needs credentials to pull back; a user's own public
	// image reference is left to the node's default behaviour.
	if target.DeploymentID != "" {
		apply.RegistryHost = registryHost
	}
	for _, p := range spec.Ports {
		if p.Container > 0 {
			apply.Ports = append(apply.Ports, p.Container)
		}
	}

	raw, _ := json.Marshal(apply)
	return []dsd.Op{{
		ID:        "res:" + rs.ResourceID,
		Kind:      dsd.KindK8sApply,
		DependsOn: deps,
		Spec:      raw,
	}}, true
}

// renderClusterComposeOps turns a Compose service graph into one workload per
// service.
//
// This is what a Compose app deployed to a cluster is: N Deployments, N
// Services, and an Ingress on the web-facing one — not a single container with
// the others silently dropped, which is what the product did before (the plain
// path reads spec.Image, a Compose app has none, so nothing rendered at all and
// the deploy sat in "deploying" forever).
//
// Per-service PLACEMENT is deliberately ignored: pinning a pod to a host is the
// scheduler's job, and honouring a serverId here would mean nodeSelectors and
// taints the product does not model. depends_on becomes real op ordering,
// because unlike the multi-server Compose path every service in a cluster
// renders into the SAME document.
func renderClusterComposeOps(rs store.ResourceSpec, spec appResourceSpec, refsSpec []secretRef, hosts []string, target store.DeployTarget, namespace string, nodeDeps []string, repository, registryHost string) (ops []dsd.Op, ok bool) {
	services := validComposeServices(spec.Compose.Services)
	if len(services) == 0 {
		return nil, false
	}
	reship := reshipsRetainedImage(target, true)
	// One service that builds from source is enough to need a registry: the
	// graph deploys together or not at all.
	buildsFromSource := false
	for _, s := range services {
		if s.Build != "" {
			buildsFromSource = true
			break
		}
	}
	if buildsFromSource {
		if render, _ := clusterImageReady(target, repository, reship); !render {
			return nil, false
		}
	}

	// Every workload name up front — the DECLARED set, not the rendered one, so a
	// service still waiting on its build is not pruned away by the services that
	// are ahead of it. Each op carries the full list; the agent reconciles its
	// manifest directory down to exactly these.
	names := make([]string, 0, len(services))
	nameOf := make(map[string]string, len(services))
	for _, s := range services {
		n := dsd.K8sWorkloadName(rs.ResourceID, s.Name)
		if n == "" {
			return nil, false
		}
		nameOf[s.Name] = n
		names = append(names, n)
	}
	sort.Strings(names)

	// The web-facing service carries the ingress: the first source-built service
	// with a port (the app, not a ported infra image), else the first with a port.
	// Same rule as the server path, so attaching a domain routes to the same
	// service whether the app runs on a server or in a cluster.
	webSvc := ""
	for _, s := range services {
		if len(s.Ports) > 0 && s.Build != "" && s.Rollout != gitRolloutRecreate {
			webSvc = s.Name
			break
		}
	}
	if webSvc == "" {
		for _, s := range services {
			if len(s.Ports) > 0 {
				webSvc = s.Name
				break
			}
		}
	}

	// Per-service build gate. The builds run in the build server's document and
	// report per service, so applying a manifest the moment the FIRST build lands
	// would point the other services at tags that do not exist yet. A service
	// renders once its own build has reported; the deployment's per-service
	// status is the same signal the cross-server Compose path gates on.
	renderable := make([]composeServiceSpec, 0, len(services))
	for _, s := range services {
		if s.Build == "" || reship {
			renderable = append(renderable, s)
			continue
		}
		switch target.ServiceStatus[s.Name] {
		case "deploying", "success":
			renderable = append(renderable, s)
		}
	}
	if len(renderable) == 0 {
		return nil, false
	}
	rendering := make(map[string]bool, len(renderable))
	for _, s := range renderable {
		rendering[s.Name] = true
	}

	for _, s := range renderable {
		image := s.Image
		if s.Build != "" {
			image = dsd.QualifyImage(repository,
				dsd.PinnedServiceImageTag(rs.ResourceID, s.Name, target.SHA, target.ImagePin))
		}
		if image == "" {
			continue
		}
		apply := k8sApplyOpSpec{
			ResourceID:   rs.ResourceID,
			Service:      s.Name,
			Name:         nameOf[s.Name],
			Namespace:    namespace,
			Image:        image,
			Replicas:     defaultClusterReplicas,
			Env:          mergeEnv(spec.Env, s.Env),
			SecretRefs:   refsSpec,
			DeploymentID: target.DeploymentID,
			Workloads:    names,
		}
		if s.Build != "" {
			apply.RegistryHost = registryHost
		}
		if s.Name == webSvc {
			apply.Hosts = hosts
		}
		apply.Ports = append(apply.Ports, s.Ports...)

		deps := append([]string(nil), nodeDeps...)
		for _, d := range s.DependsOn {
			// Only services rendered in THIS document: a typo'd dependency, or one
			// still waiting on its own build, must not become a dangling op
			// reference — that wedges the entire apply, not just this workload.
			if rendering[d] && d != s.Name {
				deps = append(deps, "res:"+rs.ResourceID+":"+d)
			}
		}
		raw, _ := json.Marshal(apply)
		ops = append(ops, dsd.Op{
			// The per-service op id the status path already understands: it routes
			// to AdvanceDeploymentService and aggregates into one resource status.
			ID:        "res:" + rs.ResourceID + ":" + s.Name,
			Kind:      dsd.KindK8sApply,
			DependsOn: deps,
			Spec:      raw,
		})
	}
	return ops, len(ops) > 0
}

// renderClusterBuildOps emits the clone + build pipeline for a cluster workload
// into the BUILD SERVER's document.
//
// A cluster resource has no server of its own, so nothing else renders these:
// before this the manifest referenced an image tag that no machine had ever
// been asked to produce. The build always pushes — the whole point is that the
// nodes, not this machine, run the result.
func renderClusterBuildOps(rs store.ResourceSpec, target store.DeployTarget, repository string) (ops []dsd.Op, ok bool) {
	if target.DeploymentID == "" || repository == "" {
		return nil, false
	}
	var spec appResourceSpec
	if err := json.Unmarshal(rs.Spec, &spec); err != nil {
		return nil, false
	}
	isCompose := spec.Compose != nil && len(spec.Compose.Services) > 0
	if reshipsRetainedImage(target, isCompose) {
		return nil, false // the images are already in the registry
	}

	cloneID := "clone:" + rs.ResourceID
	clone, _ := json.Marshal(gitCloneOpSpec{
		ResourceID: rs.ResourceID, Provider: target.Provider, RepoFullName: target.RepoFullName,
		Ref: target.Ref, SHA: target.SHA, CredentialRef: target.DeploymentID,
	})

	if !isCompose {
		// The build config the wizard collected — auto-build vs Dockerfile, the
		// dockerfile path, the context subdirectory — travels here exactly as it
		// does on the single-server path. Omitting it built every cluster app as
		// "Dockerfile at the clone root", so a monorepo built the wrong service
		// and a nixpacks app failed with "build context missing Dockerfile" —
		// the one error the auto-build path exists to make unreachable. The
		// per-service branch below already carried it; only this one was blind.
		builder, dockerfile, contextSubdir := buildConfig(spec.Build)
		build, _ := json.Marshal(buildImageOpSpec{
			ResourceID: rs.ResourceID, SHA: target.SHA, DedupKey: target.ConfigHash + ":" + target.SHA,
			ImageTag:      dsd.QualifyImage(repository, dsd.PinnedImageTag(rs.ResourceID, target.SHA, target.ImagePin)),
			DeploymentID:  target.DeploymentID,
			Builder:       builder,
			Dockerfile:    dockerfile,
			ContextSubdir: contextSubdir,
			Force:         manualForce(target),
			PushImage:     true,
		})
		return []dsd.Op{
			{ID: cloneID, Kind: dsd.KindGitClone, Spec: clone},
			{ID: "build:" + rs.ResourceID, Kind: dsd.KindImageBuild, DependsOn: []string{cloneID}, Spec: build},
		}, true
	}

	for _, s := range validComposeServices(spec.Compose.Services) {
		if s.Build == "" {
			continue // a prebuilt image is pulled by the nodes directly
		}
		svcKey := rs.ResourceID + ":" + s.Name
		build, _ := json.Marshal(buildImageOpSpec{
			ResourceID:    rs.ResourceID,
			SHA:           target.SHA,
			DedupKey:      target.ConfigHash + ":" + s.Name + ":" + target.SHA,
			Dockerfile:    s.Dockerfile,
			ContextSubdir: s.Build,
			ImageTag: dsd.QualifyImage(repository,
				dsd.PinnedServiceImageTag(rs.ResourceID, s.Name, target.SHA, target.ImagePin)),
			DeploymentID: target.DeploymentID,
			Force:        manualForce(target),
			PushImage:    true,
		})
		ops = append(ops, dsd.Op{ID: "build:" + svcKey, Kind: dsd.KindImageBuild, DependsOn: []string{cloneID}, Spec: build})
	}
	if len(ops) == 0 {
		return nil, false // every service is prebuilt: nothing to clone for
	}
	return append([]dsd.Op{{ID: cloneID, Kind: dsd.KindGitClone, Spec: clone}}, ops...), true
}
