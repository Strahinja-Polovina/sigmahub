package reconciler

import (
	"encoding/json"

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
	ResourceID   string `json:"resourceId"`
	SHA          string `json:"sha"`
	DedupKey     string `json:"dedupKey"`
	Dockerfile   string `json:"dockerfile,omitempty"`
	ImageTag     string `json:"imageTag"`
	DeploymentID string `json:"deploymentId,omitempty"`
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

// shortSHA is the 8-char generation tag; git SHAs are validated upstream.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
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
	if len(domains) > 0 {
		lbPort := 0
		if len(spec.Ports) > 0 {
			lbPort = spec.Ports[0].Container
		}
		cs.Labels = traefikLabels(rs.ResourceID, domains, lbPort)
	}

	rollout := rolloutOpSpec{
		Container:    cs,
		Generation:   shortSHA(target.SHA),
		Health:       deployHealth(spec, rs.Spec),
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
	})
	ops = append(ops,
		dsd.Op{ID: cloneID, Kind: dsd.KindGitClone, Spec: clone},
		dsd.Op{ID: buildID, Kind: dsd.KindImageBuild, DependsOn: []string{cloneID}, Spec: build},
		dsd.Op{ID: rolloutID, Kind: dsd.KindDeployRollout, DependsOn: []string{networkID, buildID}, Spec: rolloutBytes},
	)
	return ops, networkID, true
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
