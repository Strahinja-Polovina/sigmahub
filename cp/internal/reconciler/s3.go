package reconciler

// P2-1: S3 storage resources render exactly like databases — a pinned engine
// image, a data volume, and a container publishing the S3 API only on the
// server's mesh address. Secrets ride as references; the DSD never carries
// the root key.

import (
	"encoding/json"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// renderS3Ops fans an S3 resource into image.pull + volume.ensure +
// container.apply. ok=false (pre-mesh, unknown engine) falls back to the
// resource.sync stub, mirroring renderDatabaseOps.
func renderS3Ops(rs store.ResourceSpec, target store.S3Target, meshIP string) (ops []dsd.Op, networkID string, ok bool) {
	// The engine is authoritative from the credentials row (target.Engine), not
	// the kind — one `s3` kind can carry either MinIO or SeaweedFS (P2-2).
	def, isEngine := store.S3EngineByName(target.Engine)
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
		Env:        def.PlainEnv(target.AccessKey),
		// Mesh-only exposure, same invariant as databases: the S3 API is
		// reachable org-mesh-wide and nowhere else.
		Ports:   []portMapping{{Container: def.APIPort, Host: target.Port, HostIP: meshIP}},
		Volumes: []volumeMount{{Name: dataVol, MountPath: def.DataMount}},
		Command: def.Command(),
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
