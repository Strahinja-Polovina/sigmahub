package reconciler

import (
	"encoding/json"
	"strings"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// gitCloneOpSpec / buildImageOpSpec / rolloutOpSpec mirror the agent's build +
// container package JSON exactly — the wire contract for the deploy pipeline.

type gitCloneOpSpec struct {
	ResourceID    string `json:"resourceId"`
	Provider      string `json:"provider"`
	RepoFullName  string `json:"repoFullName"`
	Ref           string `json:"ref"`
	SHA           string `json:"sha"`
	CredentialRef string `json:"credentialRef,omitempty"`
}

type buildImageOpSpec struct {
	ResourceID string `json:"resourceId"`
	SHA        string `json:"sha"`
	DedupKey   string `json:"dedupKey"`
	Dockerfile string `json:"dockerfile,omitempty"`
	// ContextSubdir is the build context relative to the cloned repo root — a
	// Compose service's `build:` path (empty ⇒ repo root).
	ContextSubdir string `json:"contextSubdir,omitempty"`
	ImageTag      string `json:"imageTag"`
	DeploymentID  string `json:"deploymentId,omitempty"`
	// Force skips the build-dedup short-circuit so a manual redeploy rebuilds the
	// same commit (e.g. to pick up base-image changes) instead of reusing the cached image.
	Force bool `json:"force,omitempty"`
}

type healthProbe struct {
	Type        string `json:"type"`
	Path        string `json:"path,omitempty"`
	Port        int    `json:"port,omitempty"`
	IntervalSec int    `json:"intervalSec,omitempty"`
	TimeoutSec  int    `json:"timeoutSec,omitempty"`
}

type rolloutOpSpec struct {
	Container    containerOpSpec `json:"container"`
	Generation   string          `json:"generation"`
	Health       healthProbe     `json:"health"`
	DeploymentID string          `json:"deploymentId,omitempty"`
}

// detectedHealth is the shape gitdetect stores in a resource spec's health field.
type detectedHealth struct {
	Type        string `json:"type"`
	Path        string `json:"path"`
	Port        int    `json:"port"`
	IntervalSec int    `json:"intervalSec"`
}

// shortSHA is the 8-char SHA prefix; git SHAs are validated upstream.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// deployGeneration is the per-deployment rollout generation: the short SHA plus a
// slice of the deployment id. Making it unique PER DEPLOYMENT (not just per SHA)
// means every deploy — including a manual redeploy or a config-only change on the
// same commit — rolls out under a distinct container name, so the agent always
// creates-and-health-gates the new generation BEFORE draining the old (never a
// hard cut), and a rollout re-apply stays idempotent on that name.
func deployGeneration(sha, deploymentID string) string {
	suffix := deploymentID
	if i := strings.LastIndex(suffix, "_"); i >= 0 {
		suffix = suffix[i+1:]
	}
	if len(suffix) > 6 {
		suffix = suffix[len(suffix)-6:]
	}
	if suffix == "" {
		return shortSHA(sha)
	}
	return shortSHA(sha) + "-" + suffix
}

// renderDeployOps expands a git-deployed app resource into its deploy pipeline:
// git.clone → image.build → deploy.rollout (a rollback whose image is already
// built skips clone+build and renders only the rollout). The rollout container
// runs the built image, publishes NO host ports (Traefik routes it internally so
// two generations never conflict), and carries the router labels + health probe.
func renderDeployOps(rs store.ResourceSpec, refs []store.SecretRefMeta, domains []store.Domain, target store.DeployTarget) (ops []dsd.Op, networkID string, ok bool) {
	var spec appResourceSpec
	if err := json.Unmarshal(rs.Spec, &spec); err != nil {
		return nil, "", false
	}
	// A Compose app deploys its service graph: one shared clone, then a build +
	// rollout/recreate op per service.
	if spec.Compose != nil && len(spec.Compose.Services) > 0 {
		return renderComposeDeployOps(rs, spec, refs, domains, target)
	}

	networkName := dsd.NetworkName(rs.ProjectID)
	networkID = "net:" + rs.ProjectID
	imageTag := dsd.DeployImageTag(rs.ResourceID, target.SHA)

	// Container spec for the rollout: the built image, no host-port publishing.
	cs := containerOpSpec{
		ResourceID:     rs.ResourceID,
		Name:           dsd.ContainerName(rs.ResourceID),
		Image:          imageTag,
		Network:        networkName,
		Env:            spec.Env,
		Command:        spec.Command,
		User:           spec.User,
		ReadOnlyRootfs: spec.ReadOnlyRootfs,
		Tmpfs:          spec.Tmpfs,
		CPUs:           spec.CPUs,
		MemoryMB:       spec.MemoryMB,
		Restart:        spec.Restart,
	}
	// Expose (not publish) the declared ports so Traefik can reach the container.
	for _, p := range spec.Ports {
		cs.Ports = append(cs.Ports, portMapping{Container: p.Container})
	}
	fileMode := false
	for _, r := range refs {
		cs.SecretRefs = append(cs.SecretRefs, secretRef{Name: r.Name, EnvVar: r.EnvVar})
		if !r.EnvVar {
			fileMode = true
		}
	}
	if fileMode {
		cs.Tmpfs = append(cs.Tmpfs, secretsMountDir)
	}
	health := deployHealth(spec, rs.Spec)
	if len(domains) > 0 {
		lbPort := 0
		if len(spec.Ports) > 0 {
			lbPort = spec.Ports[0].Container
		}
		cs.Labels = traefikLabels(rs.ResourceID, domains, lbPort)
		// Gated attach: two generations of a git-deployed app share the same
		// Traefik router labels, so the LB must withhold traffic from the new
		// generation until it is healthy. A service-level health check makes Traefik
		// probe each backend and route only to ready ones — closing the window
		// between "new container started" and "agent health gate passed".
		for k, v := range traefikHealthLabels(rs.ResourceID, health) {
			cs.Labels[k] = v
		}
	}

	rollout := rolloutOpSpec{
		Container:    cs,
		Generation:   deployGeneration(target.SHA, target.DeploymentID),
		Health:       health,
		DeploymentID: target.DeploymentID,
	}
	rolloutBytes, _ := json.Marshal(rollout)
	rolloutID := "res:" + rs.ResourceID // keeps the res: id so status write-back maps to the resource

	// A rollback reuses a retained image: skip clone + build.
	if target.Trigger == "rollback" && target.ImageDigest != "" {
		ops = append(ops, dsd.Op{ID: rolloutID, Kind: dsd.KindDeployRollout, DependsOn: []string{networkID}, Spec: rolloutBytes})
		return ops, networkID, true
	}

	cloneID := "clone:" + rs.ResourceID
	buildID := "build:" + rs.ResourceID
	clone, _ := json.Marshal(gitCloneOpSpec{
		ResourceID: rs.ResourceID, Provider: target.Provider, RepoFullName: target.RepoFullName,
		Ref: target.Ref, SHA: target.SHA, CredentialRef: target.DeploymentID,
	})
	build, _ := json.Marshal(buildImageOpSpec{
		ResourceID: rs.ResourceID, SHA: target.SHA, DedupKey: target.ConfigHash + ":" + target.SHA,
		ImageTag: imageTag, DeploymentID: target.DeploymentID,
		// A manual redeploy forces a rebuild of the same commit; git-triggered
		// deploys keep the warm-cache dedup.
		Force: target.Trigger == "manual",
	})
	ops = append(ops,
		dsd.Op{ID: cloneID, Kind: dsd.KindGitClone, Spec: clone},
		dsd.Op{ID: buildID, Kind: dsd.KindImageBuild, DependsOn: []string{cloneID}, Spec: build},
		dsd.Op{ID: rolloutID, Kind: dsd.KindDeployRollout, DependsOn: []string{networkID, buildID}, Spec: rolloutBytes},
	)
	return ops, networkID, true
}

// renderComposeDeployOps expands a Compose app resource into its per-service
// pipeline: one shared git.clone (the whole repo), then for each service a build
// (its `build:` context subdir) or image.pull (prebuilt), and a deploy.rollout
// (stateless, blue-green) or deploy.recreate (holds an exclusive resource). Each
// service container joins the shared project network under its service name so
// siblings resolve each other; depends_on becomes op ordering. Op ids carry the
// service (`build:<res>:<svc>`, `res:<res>:<svc>`) so status routes per service.
func renderComposeDeployOps(rs store.ResourceSpec, spec appResourceSpec, refs []store.SecretRefMeta, domains []store.Domain, target store.DeployTarget) (ops []dsd.Op, networkID string, ok bool) {
	networkName := dsd.NetworkName(rs.ProjectID)
	networkID = "net:" + rs.ProjectID
	gen := deployGeneration(target.SHA, target.DeploymentID)

	// Resource-scoped secret refs, applied to every service container.
	var refsSpec []secretRef
	fileMode := false
	for _, r := range refs {
		refsSpec = append(refsSpec, secretRef{Name: r.Name, EnvVar: r.EnvVar})
		if !r.EnvVar {
			fileMode = true
		}
	}

	// Clone the whole repo once — the shared build-context root for all services.
	cloneID := "clone:" + rs.ResourceID
	clone, _ := json.Marshal(gitCloneOpSpec{
		ResourceID: rs.ResourceID, Provider: target.Provider, RepoFullName: target.RepoFullName,
		Ref: target.Ref, SHA: target.SHA, CredentialRef: target.DeploymentID,
	})
	ops = append(ops, dsd.Op{ID: cloneID, Kind: dsd.KindGitClone, Spec: clone})

	// The web-facing service (first that exposes a port) carries the Traefik router
	// labels when a domain is attached to the resource.
	webSvc := ""
	for _, s := range spec.Compose.Services {
		if len(s.Ports) > 0 {
			webSvc = s.Name
			break
		}
	}

	for _, s := range spec.Compose.Services {
		svcKey := rs.ResourceID + ":" + s.Name
		cs := containerOpSpec{
			ResourceID:     rs.ResourceID,
			Service:        s.Name,
			Name:           dsd.ServiceContainerName(rs.ResourceID, s.Name),
			Network:        networkName,
			NetworkAliases: []string{s.Name},
			Env:            spec.Env,
			Restart:        spec.Restart,
			SecretRefs:     refsSpec,
		}
		for _, p := range s.Ports {
			cs.Ports = append(cs.Ports, portMapping{Container: p})
		}
		if fileMode {
			cs.Tmpfs = append(cs.Tmpfs, secretsMountDir)
		}
		if s.Name == webSvc && len(domains) > 0 {
			lbPort := 0
			if len(s.Ports) > 0 {
				lbPort = s.Ports[0]
			}
			cs.Labels = traefikLabels(rs.ResourceID, domains, lbPort)
		}

		// Image source: build the service's context, or pull a prebuilt image.
		var imageDep string
		if s.Build != "" {
			imageTag := dsd.DeployServiceImageTag(rs.ResourceID, s.Name, target.SHA)
			cs.Image = imageTag
			buildID := "build:" + svcKey
			build, _ := json.Marshal(buildImageOpSpec{
				ResourceID: rs.ResourceID, SHA: target.SHA,
				DedupKey:      target.ConfigHash + ":" + s.Name + ":" + target.SHA,
				Dockerfile:    s.Dockerfile,
				ContextSubdir: s.Build,
				ImageTag:      imageTag,
				DeploymentID:  target.DeploymentID,
				Force:         target.Trigger == "manual",
			})
			ops = append(ops, dsd.Op{ID: buildID, Kind: dsd.KindImageBuild, DependsOn: []string{cloneID}, Spec: build})
			imageDep = buildID
		} else if s.Image != "" {
			cs.Image = s.Image
			pullID := "pull:" + svcKey
			pull, _ := json.Marshal(map[string]string{"image": s.Image})
			ops = append(ops, dsd.Op{ID: pullID, Kind: dsd.KindImagePull, Spec: pull})
			imageDep = pullID
		}

		// deploy.rollout (blue-green) unless the service holds an exclusive resource.
		kind := dsd.KindDeployRollout
		if s.Rollout == gitRolloutRecreate {
			kind = dsd.KindDeployRecreate
		}
		swap, _ := json.Marshal(rolloutOpSpec{
			Container: cs, Generation: gen, Health: composeServiceHealth(s), DeploymentID: target.DeploymentID,
		})
		deps := []string{networkID}
		if imageDep != "" {
			deps = append(deps, imageDep)
		}
		for _, d := range s.DependsOn {
			deps = append(deps, "res:"+rs.ResourceID+":"+d)
		}
		ops = append(ops, dsd.Op{ID: "res:" + svcKey, Kind: kind, DependsOn: deps, Spec: swap})
	}
	return ops, networkID, true
}

// gitRolloutRecreate mirrors gitdetect.RolloutRecreate without importing it.
const gitRolloutRecreate = "recreate"

// composeServiceHealth is a service's readiness probe: a TCP probe on its first
// declared container port (the never-cut/gate always needs a probe). A service
// with no ports gets a probe on port 0, which the agent treats as "port 80".
func composeServiceHealth(s composeServiceSpec) healthProbe {
	port := 0
	if len(s.Ports) > 0 {
		port = s.Ports[0]
	}
	return healthProbe{Type: "tcp", Port: port, IntervalSec: 3, TimeoutSec: 120}
}

// deployHealth resolves the rollout's health probe: the gitdetect-detected probe
// stored in the resource spec, or a default TCP probe on the primary declared
// port (the never-cut gate always needs a probe).
func deployHealth(spec appResourceSpec, raw json.RawMessage) healthProbe {
	var wrap struct {
		Health detectedHealth `json:"healthCheck"`
	}
	_ = json.Unmarshal(raw, &wrap)
	h := wrap.Health
	primary := 0
	if len(spec.Ports) > 0 {
		primary = spec.Ports[0].Container
	}
	out := healthProbe{Type: "tcp", Port: primary, IntervalSec: 3, TimeoutSec: 120}
	if h.Type == "http" && h.Path != "" {
		out.Type, out.Path = "http", h.Path
		if h.Port > 0 {
			out.Port = h.Port
		}
	} else if h.Type == "tcp" && h.Port > 0 {
		out.Port = h.Port
	}
	return out
}
