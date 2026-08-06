package reconciler

import (
	"encoding/json"
	"strings"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// manualForce reports whether a build op should force a rebuild. Only a MANUAL
// deploy that is still IN FLIGHT forces one: once it succeeds the target lingers
// (DeployTargetsForServer keeps the latest 'success' row), and a persistently
// set Force would re-run the docker build on every unrelated DSD version bump —
// e.g. a daily backup run entering/leaving the document (SIGMA-139).
func manualForce(target store.DeployTarget) bool {
	if target.Trigger != "manual" {
		return false
	}
	switch target.Status {
	case "queued", "building", "deploying":
		return true
	default:
		return false
	}
}

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
		Force: manualForce(target),
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
// service container joins the app's per-resource network under its service name
// so siblings resolve each other; depends_on becomes op ordering. Op ids carry
// the service (`build:<res>:<svc>`, `res:<res>:<svc>`) so status routes per service.
func renderComposeDeployOps(rs store.ResourceSpec, spec appResourceSpec, refs []store.SecretRefMeta, domains []store.Domain, target store.DeployTarget) (ops []dsd.Op, networkID string, ok bool) {
	// Compose services share a PER-RESOURCE network (docker-compose semantics):
	// bare service-name aliases ("db") stay scoped to this app instead of
	// colliding across apps on the shared project network. Traefik reaches the
	// web-facing service regardless — the proxy attaches to every managed network.
	networkName := dsd.ResourceNetworkName(rs.ResourceID)
	networkID = "net:res:" + rs.ResourceID
	gen := deployGeneration(target.SHA, target.DeploymentID)

	// A service with neither a build context nor a prebuilt image cannot run;
	// filter them here AND in the store's composeServiceCount (same rule), so the
	// per-service success denominator matches what is actually rendered.
	services := validComposeServices(spec.Compose.Services)
	if len(services) == 0 {
		return nil, "", false
	}

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

	// The web-facing service carries the Traefik router labels when a domain is
	// attached: prefer the first source-built blue-green service with a port (the
	// app, not an infra dependency like a ported db image), falling back to the
	// first service with a port.
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

	// The rendered-service set backs depends_on validation below.
	rendered := map[string]bool{}
	for _, s := range services {
		rendered[s.Name] = true
	}

	for _, s := range services {
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
		// depends_on ordering — only for services that are actually rendered, so a
		// typo'd or filtered-out dependency can't produce a dangling op reference
		// (which would wedge the whole apply).
		for _, d := range s.DependsOn {
			if rendered[d] {
				deps = append(deps, "res:"+rs.ResourceID+":"+d)
			}
		}
		ops = append(ops, dsd.Op{ID: "res:" + svcKey, Kind: kind, DependsOn: deps, Spec: swap})
	}
	return ops, networkID, true
}

// gitRolloutRecreate mirrors gitdetect.RolloutRecreate without importing it.
const gitRolloutRecreate = "recreate"

// validComposeServices filters to services that can actually run: a build context
// or a prebuilt image. MUST match the store's composeServiceCount rule so the
// per-service success denominator equals the number of rendered services.
func validComposeServices(in []composeServiceSpec) []composeServiceSpec {
	out := make([]composeServiceSpec, 0, len(in))
	for _, s := range in {
		if s.Name != "" && (s.Build != "" || s.Image != "") {
			out = append(out, s)
		}
	}
	return out
}

// composeServiceHealth is a service's readiness probe: a TCP probe on its first
// declared container port. A portless service (a worker) has nothing to probe —
// type "none" makes the agent's gate treat "running" as ready instead of probing
// port 80 and failing forever.
func composeServiceHealth(s composeServiceSpec) healthProbe {
	if len(s.Ports) == 0 {
		return healthProbe{Type: "none", IntervalSec: 3, TimeoutSec: 120}
	}
	return healthProbe{Type: "tcp", Port: s.Ports[0], IntervalSec: 3, TimeoutSec: 120}
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
