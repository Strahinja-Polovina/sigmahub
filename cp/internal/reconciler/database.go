package reconciler

import (
	"encoding/json"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dbengine"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// renderDBOps expands a database resource (P1-10) into its typed ops:
// image.pull → volume.ensure (the engine's data volume) → container.apply. The
// engine's listener publishes EXCLUSIVELY on the server's mesh address (v1 is
// mesh-internal only: reachable across the org's WireGuard mesh, never from the
// public internet; P1-5's nftables keeps the public side closed as the second
// layer). The generated password rides as a secret REFERENCE (P1-6 channel);
// username/database are deterministic and non-secret. Tuning args are selected
// by the hosting server's type: prod-grade knobs on database-type servers,
// image defaults elsewhere. Returns ok=false for engines not compiled in.
func renderDBOps(rs store.ResourceSpec, hardening store.HostHardening) (ops []dsd.Op, networkID string, ok bool) {
	eng, ok := dbengine.Get(rs.Kind)
	if !ok {
		return nil, "", false
	}

	networkName := dsd.NetworkName(rs.ProjectID)
	networkID = "net:" + rs.ProjectID
	imageID := "img:" + rs.ResourceID
	containerID := "res:" + rs.ResourceID

	imgSpec, _ := json.Marshal(map[string]string{"image": eng.Image})
	ops = append(ops, dsd.Op{ID: imageID, Kind: dsd.KindImagePull, Spec: imgSpec})

	dockerVol := dsd.VolumeName(rs.ResourceID, "data")
	volID := "vol:" + rs.ResourceID + ":data"
	vs, _ := json.Marshal(map[string]string{"name": dockerVol, "resourceId": rs.ResourceID})
	ops = append(ops, dsd.Op{ID: volID, Kind: dsd.KindVolumeEnsure, Spec: vs})

	username, database := dbengine.DerivedIdentity(rs.ResourceID)
	creds := dbengine.Credentials{Username: username, Database: database}

	cs := containerOpSpec{
		ResourceID: rs.ResourceID,
		Name:       dsd.ContainerName(rs.ResourceID),
		Image:      eng.Image,
		Network:    networkName,
		Env:        eng.Env(creds),
		Volumes:    []volumeMount{{Name: dockerVol, MountPath: eng.DataPath}},
		Restart:    "unless-stopped",
	}
	// Engine command: a fixed base (e.g. Redis's wait-for-conf exec) plus the
	// profile's tuning args. For env-auth engines the base is empty and the
	// tuning args alone become the command (the image entrypoint prepends the
	// daemon binary for flag-style args).
	profile := dbengine.ProfileForServerType(hardening.ServerType)
	cs.Command = append(append([]string{}, eng.Command...), eng.Tuning(profile)...)

	// Mesh-only exposure: publish the engine port bound to the mesh address, so
	// clients on the org's WireGuard mesh reach it and nothing else can. Until
	// the server has a mesh IP, don't publish at all (project-network only).
	if hardening.MeshIP != "" {
		cs.Ports = []portMapping{{Container: eng.Port, Host: eng.Port, HostIP: hardening.MeshIP}}
	}

	// Password reference (never the value). File-mode engines (Redis) also need
	// the tmpfs secrets mount the agent seeds into.
	switch {
	case eng.Secret.EnvName != "":
		cs.SecretRefs = []secretRef{{Name: eng.Secret.EnvName, EnvVar: true}}
	case eng.Secret.FileName != "":
		cs.SecretRefs = []secretRef{{Name: eng.Secret.FileName, EnvVar: false}}
		cs.Tmpfs = append(cs.Tmpfs, secretsMountDir)
	}

	csBytes, _ := json.Marshal(cs)
	ops = append(ops, dsd.Op{ID: containerID, Kind: dsd.KindContainerApply, DependsOn: []string{networkID, imageID, volID}, Spec: csBytes})
	return ops, networkID, true
}
