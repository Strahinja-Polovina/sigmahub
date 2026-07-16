package reconciler

import (
	"encoding/json"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// appResourceSpec is the user-authored `spec` JSONB of an "app" resource. The
// control plane translates it into the typed container ops the agent applies.
type appResourceSpec struct {
	Image string            `json:"image"`
	Env   map[string]string `json:"env"`
	Ports []struct {
		Container int    `json:"container"`
		Host      int    `json:"host"`
		Protocol  string `json:"protocol"`
	} `json:"ports"`
	Volumes []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
		ReadOnly  bool   `json:"readOnly"`
	} `json:"volumes"`
	Command        []string `json:"command"`
	User           string   `json:"user"`
	ReadOnlyRootfs bool     `json:"readOnlyRootfs"`
	Tmpfs          []string `json:"tmpfs"`
	CPUs           float64  `json:"cpus"`
	MemoryMB       int64    `json:"memoryMb"`
	Restart        string   `json:"restart"`
	// Compose, when present, makes this a multi-service deploy: each service gets
	// its own build + rollout/recreate op. Populated from gitdetect at connect.
	Compose *composeDeploySpec `json:"compose,omitempty"`
}

// composeDeploySpec is the Compose service graph stored on a multi-service app
// resource — the CP renders per-service ops from it.
type composeDeploySpec struct {
	Services []composeServiceSpec `json:"services"`
}

// composeServiceSpec is one service in a Compose deploy.
type composeServiceSpec struct {
	Name       string   `json:"name"`
	Build      string   `json:"build,omitempty"`      // build-context subdir; empty ⇒ prebuilt Image
	Dockerfile string   `json:"dockerfile,omitempty"` // relative to the build context
	Image      string   `json:"image,omitempty"`      // prebuilt image ref (when no Build)
	Ports      []int    `json:"ports,omitempty"`      // container ports to expose
	Rollout    string   `json:"rollout,omitempty"`    // blue-green (default) | recreate
	DependsOn  []string `json:"dependsOn,omitempty"`
}

// The op-spec structs mirror the agent's container package JSON tags exactly.
// They are the wire contract between renderOps and the agent's op handlers.

type portMapping struct {
	Container int `json:"container"`
	Host      int `json:"host,omitempty"`
	// HostIP restricts the published port to one host address — the P1-10
	// mesh-only bind (the server's WireGuard IP). Empty publishes on all
	// interfaces (subject to the P1-5 firewall).
	HostIP   string `json:"hostIp,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

type volumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// secretRef is a reference (never a value) to a secret the agent fetches and
// injects at container-create. EnvVar false = tmpfs file under secretsMountDir.
type secretRef struct {
	Name   string `json:"name"`
	EnvVar bool   `json:"envVar,omitempty"`
}

// secretsMountDir is the in-memory (tmpfs) directory file-mode secrets are
// seeded into by the agent; shared with the agent's container package.
const secretsMountDir = "/run/secrets"

type containerOpSpec struct {
	ResourceID     string            `json:"resourceId"`
	Service        string            `json:"service,omitempty"`
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	Network        string            `json:"network"`
	NetworkAliases []string          `json:"networkAliases,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Ports          []portMapping     `json:"ports,omitempty"`
	Volumes        []volumeMount     `json:"volumes,omitempty"`
	Command        []string          `json:"command,omitempty"`
	User           string            `json:"user,omitempty"`
	ReadOnlyRootfs bool              `json:"readOnlyRootfs,omitempty"`
	Tmpfs          []string          `json:"tmpfs,omitempty"`
	CPUs           float64           `json:"cpus,omitempty"`
	MemoryMB       int64             `json:"memoryMb,omitempty"`
	Restart        string            `json:"restart,omitempty"`
	SecretRefs     []secretRef       `json:"secretRefs,omitempty"`
	// Labels are Docker container labels — the P1-8 Traefik ingress path renders
	// router labels here when the resource has an attached domain.
	Labels map[string]string `json:"labels,omitempty"`
}

// renderAppOps expands one "app" resource into its ordered container ops:
// image.pull → volume.ensure(s) → container.apply, with the container depending
// on its image, its volumes, and its project network (whose op is emitted once
// per project by the caller). Returns ok=false when the resource is not yet
// deployable (no image), so the caller falls back to a no-op resource.sync.
func renderAppOps(rs store.ResourceSpec, refs []store.SecretRefMeta, domains []store.Domain) (ops []dsd.Op, networkID string, ok bool) {
	var spec appResourceSpec
	if err := json.Unmarshal(rs.Spec, &spec); err != nil || spec.Image == "" {
		return nil, "", false
	}

	networkName := dsd.NetworkName(rs.ProjectID)
	networkID = "net:" + rs.ProjectID
	imageID := "img:" + rs.ResourceID
	containerID := "res:" + rs.ResourceID // maps to resources.status on ingest

	// image.pull
	imgSpec, _ := json.Marshal(map[string]string{"image": spec.Image})
	ops = append(ops, dsd.Op{ID: imageID, Kind: dsd.KindImagePull, Spec: imgSpec})

	// volume.ensure per declared volume; the container depends on each.
	deps := []string{networkID, imageID}
	var mounts []volumeMount
	for _, v := range spec.Volumes {
		if v.Name == "" {
			continue
		}
		dockerVol := dsd.VolumeName(rs.ResourceID, v.Name)
		volID := "vol:" + rs.ResourceID + ":" + v.Name
		vs, _ := json.Marshal(map[string]string{"name": dockerVol, "resourceId": rs.ResourceID})
		ops = append(ops, dsd.Op{ID: volID, Kind: dsd.KindVolumeEnsure, Spec: vs})
		deps = append(deps, volID)
		mounts = append(mounts, volumeMount{Name: dockerVol, MountPath: v.MountPath, ReadOnly: v.ReadOnly})
	}

	// container.apply
	cs := containerOpSpec{
		ResourceID:     rs.ResourceID,
		Name:           dsd.ContainerName(rs.ResourceID),
		Image:          spec.Image,
		Network:        networkName,
		Env:            spec.Env,
		Volumes:        mounts,
		Command:        spec.Command,
		User:           spec.User,
		ReadOnlyRootfs: spec.ReadOnlyRootfs,
		Tmpfs:          spec.Tmpfs,
		CPUs:           spec.CPUs,
		MemoryMB:       spec.MemoryMB,
		Restart:        spec.Restart,
	}
	for _, p := range spec.Ports {
		cs.Ports = append(cs.Ports, portMapping{Container: p.Container, Host: p.Host, Protocol: p.Protocol})
	}
	// Secret references (never values). File-mode secrets need a tmpfs mount at
	// secretsMountDir for the agent to seed into; add it once if any exist.
	fileMode := false
	for _, r := range refs {
		cs.SecretRefs = append(cs.SecretRefs, secretRef{Name: r.Name, EnvVar: r.EnvVar})
		if !r.EnvVar {
			fileMode = true
		}
	}
	if fileMode {
		has := false
		for _, t := range cs.Tmpfs {
			if t == secretsMountDir {
				has = true
			}
		}
		if !has {
			cs.Tmpfs = append(cs.Tmpfs, secretsMountDir)
		}
	}
	// Traefik router labels for any attached domain. The port is the container
	// port Traefik dials on the shared project network (first declared port).
	if len(domains) > 0 {
		lbPort := 0
		if len(spec.Ports) > 0 {
			lbPort = spec.Ports[0].Container
		}
		cs.Labels = traefikLabels(rs.ResourceID, domains, lbPort)
	}
	csBytes, _ := json.Marshal(cs)
	ops = append(ops, dsd.Op{ID: containerID, Kind: dsd.KindContainerApply, DependsOn: deps, Spec: csBytes})

	return ops, networkID, true
}

// renderVolumeRemoveOp renders a confirmed destructive volume removal. The op
// id carries the pending-op id so the agent's status report maps back to the
// pending_destructive_ops row.
func renderVolumeRemoveOp(p store.PendingDestructiveOp) dsd.Op {
	spec, _ := json.Marshal(map[string]string{"name": p.Target})
	return dsd.Op{ID: "volrm:" + p.ID, Kind: dsd.KindVolumeRemove, Spec: spec}
}
