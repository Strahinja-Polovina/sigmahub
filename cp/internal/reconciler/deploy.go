package reconciler

import (
	"encoding/json"

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

// crossHostRegistryHost is the registry to authenticate against, but only for a
// build whose image actually leaves this machine. A same-host build pushes
// nothing, so naming a registry there would make the agent fetch a credential
// it has no use for.
func crossHostRegistryHost(crossHost bool, registry registryRender) string {
	if !crossHost {
		return ""
	}
	return registry.host
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
	// PushImage is set when the build runs on a DEDICATED build server: the
	// deploy target can't read another host's Docker daemon, so the image has to
	// reach a registry both can see.
	PushImage bool `json:"pushImage,omitempty"`
	// RegistryHost is the host to authenticate the push against. The agent
	// fetches the credential over its own channel — the DSD carries the
	// coordinates, never the password.
	RegistryHost string `json:"registryHost,omitempty"`
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
// means every deploy rolls out under a distinct container name, so the agent
// always creates-and-health-gates the new generation BEFORE draining the old
// (never a hard cut), and a rollout re-apply stays idempotent on that name.
//
// This only covers a config-only change (domain attach, secret add/remove/value
// update) because the CP mints a 'config' deployment for it (SIGMA-166): the
// standing SUCCESS target would otherwise re-render a DIFFERENT spec under the
// SAME generation forever, and the agent's never-cut guard refuses that.
func deployGeneration(sha, deploymentID string) string {
	suffix := dsd.DeployPin(deploymentID)
	if suffix == "" {
		return shortSHA(sha)
	}
	return shortSHA(sha) + "-" + suffix
}

// reshipsRetainedImage reports whether the target re-ships an already-built
// image, skipping clone+build: a rollback, or a config deploy (SIGMA-166)
// whose source release is identifiable. requirePin is set on the Compose path,
// where a legacy source's image_digest is a tag no Compose build ever produced
// (SIGMA-168) — only the pin re-derives real per-service tags; a pinless
// Compose config row falls back to the full pipeline instead.
func reshipsRetainedImage(target store.DeployTarget, requirePin bool) bool {
	if target.Trigger != "rollback" && target.Trigger != "config" {
		return false
	}
	if requirePin {
		return target.ImagePin != ""
	}
	return target.ImagePin != "" || target.ImageDigest != ""
}

// renderDeployOps expands a git-deployed app resource into its deploy pipeline:
// git.clone → image.build → deploy.rollout (a rollback whose image is already
// built skips clone+build and renders only the rollout). The rollout container
// runs the built image, publishes NO host ports (Traefik routes it internally so
// two generations never conflict), and carries the router labels + health probe.
func renderDeployOps(rs store.ResourceSpec, refs []store.SecretRefMeta, domains []store.Domain, target store.DeployTarget, serverID string, registry registryRender) (ops []dsd.Op, networkID string, ok bool) {
	var spec appResourceSpec
	if err := json.Unmarshal(rs.Spec, &spec); err != nil {
		return nil, "", false
	}
	// A Compose app deploys its service graph: one shared clone, then a build +
	// rollout/recreate op per service.
	if spec.Compose != nil && len(spec.Compose.Services) > 0 {
		return renderComposeDeployOps(rs, spec, refs, domains, target, serverID)
	}

	networkName := dsd.NetworkName(rs.ProjectID)
	networkID = "net:" + rs.ProjectID
	// A dedicated build server means the build and the run happen on different
	// Docker daemons, so the image has to travel through the org's registry and
	// its tag has to name that registry. Without one the push would go to
	// docker.io under a namespace nobody owns and be answered with a 401 — so
	// render nothing rather than a pipeline that cannot complete.
	crossHost := target.BuildServerID != "" && target.BuildServerID != target.ServerID
	if crossHost && registry.repository == "" {
		return nil, "", false
	}
	// Per-deployment pinned tag (SIGMA-173): a building trigger tags under its
	// own pin, so no tag is ever rebuilt in place; rollback/config rows carry
	// their SOURCE release's pin, so this resolves to the exact image that
	// release built. Legacy rows (empty pin) keep the bare mutable SHA tag.
	imageTag := dsd.PinnedImageTag(rs.ResourceID, target.SHA, target.ImagePin)
	if crossHost {
		imageTag = dsd.QualifyImage(registry.repository, imageTag)
	}

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
	// Declared volumes are ensured and mounted exactly as renderAppOps does —
	// omitting them here silently unmounted an app's data the moment its first
	// git deploy landed, stranding the old bytes in an orphan volume while the
	// deployment reported success (SIGMA-169).
	var volDeps []string
	for _, v := range spec.Volumes {
		if v.Name == "" {
			continue
		}
		dockerVol := dsd.VolumeName(rs.ResourceID, v.Name)
		volID := "vol:" + rs.ResourceID + ":" + v.Name
		vs, _ := json.Marshal(map[string]string{"name": dockerVol, "resourceId": rs.ResourceID})
		ops = append(ops, dsd.Op{ID: volID, Kind: dsd.KindVolumeEnsure, Spec: vs})
		volDeps = append(volDeps, volID)
		cs.Volumes = append(cs.Volumes, volumeMount{Name: dockerVol, MountPath: v.MountPath, ReadOnly: v.ReadOnly})
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
	generation := deployGeneration(target.SHA, target.DeploymentID)
	if len(domains) > 0 {
		lbPort := 0
		if len(spec.Ports) > 0 {
			lbPort = spec.Ports[0].Container
		}
		// Gated attach: the two generations of a blue-green swap each get their own
		// router and single-server service, and the incoming one is rendered at a
		// lower priority so it cannot match a request until the agent has drained
		// its predecessor. Before SIGMA-164 both generations declared the SAME
		// router and service, which Traefik merged into one weighted service —
		// half of live traffic hit the new container from the moment it started,
		// throughout the health gate.
		bg := blueGreenRouting{Generation: generation, Priority: generationRouterPriority(target.CreatedAt)}
		cs.Labels = traefikLabels(rs.ResourceID, domains, lbPort, bg)
		// A service-level health check on top of that, when the app declares an
		// HTTP probe: it keeps a container that is up but not yet serving out of
		// its own service too, so the flip after the drain is clean.
		for k, v := range traefikHealthLabels(dsd.TraefikGenerationRouterName(rs.ResourceID, generation), health) {
			cs.Labels[k] = v
		}
	}

	rollout := rolloutOpSpec{
		Container:    cs,
		Generation:   generation,
		Health:       health,
		DeploymentID: target.DeploymentID,
	}
	rolloutBytes, _ := json.Marshal(rollout)
	rolloutID := "res:" + rs.ResourceID // keeps the res: id so status write-back maps to the resource

	// A volume-holding app deploys via recreate, not blue-green rollout: the
	// swap's overlap window would mount the same named volume into two live
	// generations at once — the same exclusivity rule that classes a Compose
	// service with named volumes as 'recreate' (SIGMA-169).
	rolloutKind := dsd.KindDeployRollout
	if len(cs.Volumes) > 0 {
		rolloutKind = dsd.KindDeployRecreate
	}
	rolloutDeps := append([]string{networkID}, volDeps...)

	// A rollback — or a config deploy re-shipping the running release
	// (SIGMA-166) — reuses a retained image: skip clone + build.
	if reshipsRetainedImage(target, false) {
		ops = append(ops, dsd.Op{ID: rolloutID, Kind: rolloutKind, DependsOn: rolloutDeps, Spec: rolloutBytes})
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
		// Where the built image must end up. On a dedicated build server the
		// deploy target cannot read the local Docker daemon of another host, so
		// the image is pushed to the registry the target then pulls from.
		PushImage:    crossHost,
		RegistryHost: crossHostRegistryHost(crossHost, registry),
	})

	// Dedicated build server: the clone + build land in ITS document, and the
	// deploy target renders only the rollout. Splitting the pipeline this way
	// keeps a build — the most CPU- and disk-hungry thing we run — off the
	// machine that is serving traffic.
	if bs := target.BuildServerID; bs != "" && bs != target.ServerID {
		if serverID == bs {
			return []dsd.Op{
				{ID: cloneID, Kind: dsd.KindGitClone, Spec: clone},
				{ID: buildID, Kind: dsd.KindImageBuild, DependsOn: []string{cloneID}, Spec: build},
			}, "", true
		}
		// The deploy target. The build op lives in another document, so it can't
		// be an op dependency here (a dangling reference would wedge the apply):
		// hold the rollout until the deployment has moved past building, exactly
		// as a cross-server Compose dependency is gated.
		if target.Status == "queued" || target.Status == "building" {
			return nil, "", false
		}
		// Pull the image the build server pushed, then roll out.
		pullID := "pull:" + rs.ResourceID
		pull, _ := json.Marshal(map[string]string{"image": imageTag})
		ops = append(ops,
			dsd.Op{ID: pullID, Kind: dsd.KindImagePull, Spec: pull},
			dsd.Op{ID: rolloutID, Kind: rolloutKind, DependsOn: append(rolloutDeps, pullID), Spec: rolloutBytes},
		)
		return ops, networkID, true
	}

	ops = append(ops,
		dsd.Op{ID: cloneID, Kind: dsd.KindGitClone, Spec: clone},
		dsd.Op{ID: buildID, Kind: dsd.KindImageBuild, DependsOn: []string{cloneID}, Spec: build},
		dsd.Op{ID: rolloutID, Kind: rolloutKind, DependsOn: append(rolloutDeps, buildID), Spec: rolloutBytes},
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
// Note on registries: a Compose service builds on the server that RUNS it (each
// document clones the repo and builds its own services), so no image crosses a
// host boundary here and no registry is involved. The dedicated build server is
// a single-container-path option for the same reason.
func renderComposeDeployOps(rs store.ResourceSpec, spec appResourceSpec, refs []store.SecretRefMeta, domains []store.Domain, target store.DeployTarget, serverID string) (ops []dsd.Op, networkID string, ok bool) {
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

	// A rollback or config deploy with a pinned source re-ships the retained
	// per-service images: NO clone, NO builds. Before SIGMA-168 a Compose
	// rollback unconditionally re-cloned at the target SHA, so the product's
	// own escape hatch depended on a live git credential and a still-reachable
	// commit — the two things most likely to be false mid-incident — while the
	// UI promised "no rebuild".
	reship := reshipsRetainedImage(target, true)

	cloneID := "clone:" + rs.ResourceID

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

	// Placement. A service with no explicit serverId lives on the deployment's
	// own server, so a plain single-server Compose app renders exactly as before.
	homeServer := target.ServerID
	placedOn := func(svc composeServiceSpec) string {
		if svc.ServerID != "" {
			return svc.ServerID
		}
		return homeServer
	}
	// declared is every runnable service across the WHOLE app (all servers) —
	// depends_on is validated against it so a dependency on a service hosted
	// elsewhere is honoured rather than silently dropped.
	declared := map[string]bool{}
	// local is what THIS document renders; only these can be ordered with
	// op-level DependsOn, because ops in another server's document are invisible
	// to this one's dependency graph.
	local := map[string]bool{}
	for _, s := range services {
		declared[s.Name] = true
		if placedOn(s) == serverID {
			local[s.Name] = true
		}
	}

	// Which of this server's services may render right now.
	//
	// Cross-server dependency gate: within one document depends_on is op
	// ordering, but across documents there is no ordering to express, so the
	// control plane holds a dependent service back until its remote dependencies
	// report success. Status ingest re-triggers a reconcile, so the service
	// renders on a later pass — an app is never started against a database that
	// isn't up yet.
	var renderable []composeServiceSpec
	for _, s := range services {
		if placedOn(s) != serverID {
			continue // another server's document renders it
		}
		gated := false
		for _, d := range s.DependsOn {
			if !declared[d] || local[d] {
				continue // unknown (ignored, as before) or ordered locally
			}
			if target.ServiceStatus[d] != "success" {
				gated = true
				break
			}
		}
		if !gated {
			renderable = append(renderable, s)
		}
	}
	// Nothing to do here: no services placed on this server, or every one of
	// them is still waiting on a remote dependency. Report not-ok so the caller
	// falls through to its stub instead of emitting a clone and a bare network.
	if len(renderable) == 0 {
		return nil, "", false
	}

	// Clone the repo once — the shared build-context root. Only when something
	// here actually builds from source: a server hosting only prebuilt images
	// has no reason to pull the repo (or to need a git credential).
	needsClone := false
	for _, s := range renderable {
		if s.Build != "" {
			needsClone = true
			break
		}
	}
	if !reship && needsClone {
		clone, _ := json.Marshal(gitCloneOpSpec{
			ResourceID: rs.ResourceID, Provider: target.Provider, RepoFullName: target.RepoFullName,
			Ref: target.Ref, SHA: target.SHA, CredentialRef: target.DeploymentID,
		})
		ops = append(ops, dsd.Op{ID: cloneID, Kind: dsd.KindGitClone, Spec: clone})
	}

	for _, s := range renderable {

		svcKey := rs.ResourceID + ":" + s.Name
		cs := containerOpSpec{
			ResourceID:     rs.ResourceID,
			Service:        s.Name,
			Name:           dsd.ServiceContainerName(rs.ResourceID, s.Name),
			Network:        networkName,
			NetworkAliases: []string{s.Name},
			Env:            mergeEnv(spec.Env, s.Env),
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
			// Generation-scoped router + priority, exactly as the single-container
			// path (SIGMA-164) — a Compose web service is blue-green too unless it
			// opted into recreate, and a recreate service has no second generation
			// to be confused with, so the same labels are correct either way.
			cs.Labels = traefikLabels(rs.ResourceID, domains, lbPort,
				blueGreenRouting{Generation: gen, Priority: generationRouterPriority(target.CreatedAt)})
			for k, v := range traefikHealthLabels(dsd.TraefikGenerationRouterName(rs.ResourceID, gen), composeServiceHealth(s)) {
				cs.Labels[k] = v
			}
		}

		// Image source: build the service's context, or pull a prebuilt image.
		// Built services run their per-deployment pinned tag (SIGMA-173); on a
		// re-ship (rollback/config) the pin came from the source release, so the
		// tag resolves to that release's exact bytes and no build op is emitted.
		var imageDep string
		if s.Build != "" {
			imageTag := dsd.PinnedServiceImageTag(rs.ResourceID, s.Name, target.SHA, target.ImagePin)
			cs.Image = imageTag
			if !reship {
				buildID := "build:" + svcKey
				build, _ := json.Marshal(buildImageOpSpec{
					ResourceID: rs.ResourceID, SHA: target.SHA,
					DedupKey:      target.ConfigHash + ":" + s.Name + ":" + target.SHA,
					Dockerfile:    s.Dockerfile,
					ContextSubdir: s.Build,
					ImageTag:      imageTag,
					DeploymentID:  target.DeploymentID,
					// Same in-flight gate as the single-container path: the standing
					// target keeps its 'manual' trigger after the deploy succeeds, so a
					// bare Trigger check stays true forever and re-forces a rebuild of
					// EVERY service on unrelated DSD bumps (SIGMA-175).
					Force: manualForce(target),
				})
				ops = append(ops, dsd.Op{ID: buildID, Kind: dsd.KindImageBuild, DependsOn: []string{cloneID}, Spec: build})
				imageDep = buildID
			}
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
		// depends_on ordering — only for services rendered in THIS document, so a
		// typo'd, filtered-out or remotely-placed dependency can't produce a
		// dangling op reference (which would wedge the whole apply). Remote ones
		// were already gated above.
		for _, d := range s.DependsOn {
			if local[d] {
				deps = append(deps, "res:"+rs.ResourceID+":"+d)
			}
		}
		ops = append(ops, dsd.Op{ID: "res:" + svcKey, Kind: kind, DependsOn: deps, Spec: swap})
	}
	return ops, networkID, true
}

// mergeEnv layers a service's own environment over the resource-level one.
// Services on different hosts need different values for the same key (a DB
// host, a queue URL), which a single shared map cannot express.
func mergeEnv(base, override map[string]string) map[string]string {
	if len(override) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
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
	// Never emit a port-less TCP probe: the agent rewrites port 0 to 80
	// (defaultProbe), so an app that listens anywhere else — the normal case —
	// would burn the full gate timeout and be reported as an unhealthy deploy
	// (SIGMA-160). With no known port there is nothing to probe, so gate on
	// "running" instead of on a port we invented.
	if out.Type == "tcp" && out.Port == 0 {
		out.Type = "none"
	}
	return out
}
