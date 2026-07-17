package reconciler

import (
	"encoding/json"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// renderDatabaseOps expands one database resource (P1-10) into its ordered
// container ops: image.pull → volume.ensure(data) → container.apply. The
// container publishes its port EXCLUSIVELY on the server's WireGuard mesh
// address (mesh-only v1 contract) and takes its generated credentials as
// secret references resolved agent-side — a captured DSD carries no secret.
// Returns ok=false until the server has a mesh address (pre-enrollment), so
// the caller falls back to the resource.sync stub rather than publishing the
// database on an undefined interface.
func renderDatabaseOps(rs store.ResourceSpec, target store.DBTarget, meshIP string) (ops []dsd.Op, networkID string, ok bool) {
	def, isEngine := store.DBEngine(rs.Kind)
	if !isEngine || meshIP == "" || target.Port == 0 {
		return nil, "", false
	}

	networkName := dsd.NetworkName(rs.ProjectID)
	networkID = "net:" + rs.ProjectID
	imageID := "img:" + rs.ResourceID
	volID := "vol:" + rs.ResourceID + ":data"
	containerID := "res:" + rs.ResourceID // maps to resources.status on ingest

	imgSpec, _ := json.Marshal(map[string]string{"image": def.Image})
	ops = append(ops, dsd.Op{ID: imageID, Kind: dsd.KindImagePull, Spec: imgSpec})

	dataVol := dsd.VolumeName(rs.ResourceID, "data")
	vs, _ := json.Marshal(map[string]string{"name": dataVol, "resourceId": rs.ResourceID})
	ops = append(ops, dsd.Op{ID: volID, Kind: dsd.KindVolumeEnsure, Spec: vs})

	cs := containerOpSpec{
		ResourceID: rs.ResourceID,
		Name:       dsd.ContainerName(rs.ResourceID),
		Image:      def.Image,
		Network:    networkName,
		Env:        def.PlainEnv(target.Username, target.Database),
		// Mesh-only exposure: bind the host side of the mapping to the mesh IP.
		// nftables (P1-5) keeps the public interfaces closed; the mesh interface
		// is always allowed, so the port is reachable org-mesh-wide and nowhere else.
		Ports:   []portMapping{{Container: def.ContainerPort, Host: target.Port, HostIP: meshIP}},
		Volumes: []volumeMount{{Name: dataVol, MountPath: def.DataMount}},
		// Server-type tuning profile: container-level engine knobs only.
		Command: def.TunedCommand(target.ServerType),
	}
	deps := []string{networkID, imageID, volID}

	// P2-5 WAL archiving: postgres archives completed segments into a spool
	// volume (tmp+rename so a half-written file is never shipped); the agent's
	// shipper drains it into the resource's restic repo. Config rides the
	// command, so flipping PITR recreates the container with archiving on.
	if target.PITR && rs.Kind == "postgres" {
		walVol := dsd.VolumeName(rs.ResourceID, "wal")
		walVolID := "vol:" + rs.ResourceID + ":wal"
		ws, _ := json.Marshal(map[string]string{"name": walVol, "resourceId": rs.ResourceID})
		ops = append(ops, dsd.Op{ID: walVolID, Kind: dsd.KindVolumeEnsure, Spec: ws})
		const spool = "/var/lib/postgresql/wal-archive"
		cs.Volumes = append(cs.Volumes, volumeMount{Name: walVol, MountPath: spool})
		cs.Command = append(cs.Command,
			"-c", "wal_level=replica",
			"-c", "archive_mode=on",
			"-c", "archive_timeout=300",
			"-c", "archive_command=test ! -f "+spool+"/%f && cp %p "+spool+"/%f.tmp && mv "+spool+"/%f.tmp "+spool+"/%f",
		)
		deps = append(deps, walVolID)
	}

	for _, name := range def.SecretEnvNames {
		cs.SecretRefs = append(cs.SecretRefs, secretRef{Name: name, EnvVar: true})
	}
	csBytes, _ := json.Marshal(cs)
	ops = append(ops, dsd.Op{
		ID:        containerID,
		Kind:      dsd.KindContainerApply,
		DependsOn: deps,
		Spec:      csBytes,
	})
	return ops, networkID, true
}
