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
	for _, name := range def.SecretEnvNames {
		cs.SecretRefs = append(cs.SecretRefs, secretRef{Name: name, EnvVar: true})
	}
	csBytes, _ := json.Marshal(cs)
	ops = append(ops, dsd.Op{
		ID:        containerID,
		Kind:      dsd.KindContainerApply,
		DependsOn: []string{networkID, imageID, volID},
		Spec:      csBytes,
	})
	return ops, networkID, true
}
