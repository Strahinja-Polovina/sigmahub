package reconciler

// GPU model hosting. An `llm` resource renders like a database — a pinned
// runtime image, a cache volume for the weights, and a container publishing the
// inference API only on the server's mesh address — plus the GPU request that
// makes it actually use the hardware.

import (
	"encoding/json"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// llmResourceSpec is the slice of an llm resource's spec we render from.
type llmResourceSpec struct {
	Engine string `json:"engine"`
	// Model is the runtime's model reference (a Hugging Face id for vLLM, a tag
	// for ollama).
	Model string `json:"model"`
	// GPUs is how many devices to request; 0 means "all on this host", which is
	// what a dedicated inference box almost always wants.
	GPUs int               `json:"gpus"`
	Env  map[string]string `json:"env"`
}

// renderLLMOps expands an llm resource into image.pull → volume.ensure →
// container.apply. Returns ok=false before mesh enrollment, so the caller falls
// back to the resource.sync stub rather than publishing an inference endpoint
// on an undefined interface (same contract as databases and object storage).
func renderLLMOps(rs store.ResourceSpec, meshIP string, port int, refs []store.SecretRefMeta) (ops []dsd.Op, networkID string, ok bool) {
	if meshIP == "" || port == 0 {
		return nil, "", false
	}
	var spec llmResourceSpec
	if err := json.Unmarshal(rs.Spec, &spec); err != nil {
		return nil, "", false
	}
	engine := spec.Engine
	if engine == "" {
		engine = store.DefaultLLMEngine
	}
	def, known := store.LLMEngine(engine)
	if !known {
		return nil, "", false
	}

	networkName := dsd.NetworkName(rs.ProjectID)
	networkID = "net:" + rs.ProjectID
	imageID := "img:" + rs.ResourceID
	volID := "vol:" + rs.ResourceID + ":models"
	containerID := "res:" + rs.ResourceID

	imgSpec, _ := json.Marshal(map[string]string{"image": def.Image})
	ops = append(ops, dsd.Op{ID: imageID, Kind: dsd.KindImagePull, Spec: imgSpec})

	// Weights are large and slow to fetch; a named volume means a redeploy
	// doesn't re-download tens of gigabytes.
	cacheVol := dsd.VolumeName(rs.ResourceID, "models")
	vs, _ := json.Marshal(map[string]string{"name": cacheVol, "resourceId": rs.ResourceID})
	ops = append(ops, dsd.Op{ID: volID, Kind: dsd.KindVolumeEnsure, Spec: vs})

	env := def.PlainEnv(spec.Model)
	if env == nil {
		env = map[string]string{}
	}
	for k, v := range spec.Env {
		env[k] = v
	}

	cs := containerOpSpec{
		ResourceID: rs.ResourceID,
		Name:       dsd.ContainerName(rs.ResourceID),
		Image:      def.Image,
		Network:    networkName,
		Env:        env,
		// Mesh-only exposure. An inference endpoint is expensive to run and
		// trivially abusable if reachable publicly, so it gets exactly the
		// treatment a database gets.
		Ports:   []portMapping{{Container: def.ContainerPort, Host: port, HostIP: meshIP}},
		Volumes: []volumeMount{{Name: cacheVol, MountPath: def.ModelCacheMount}},
		Command: def.Command(spec.Model),
		// Without this the runtime silently falls back to CPU and serves tokens
		// at a useless rate on hardware the customer is paying a lot for.
		GPUs: gpuRequest(spec.GPUs),
		// Model loading maps the whole file; the runtime images document a
		// large shared-memory segment as a requirement.
		ShmSizeMB: llmShmSizeMB,
	}
	for _, name := range def.SecretEnvNames {
		cs.SecretRefs = append(cs.SecretRefs, secretRef{Name: name, EnvVar: true})
	}
	// Resource-scoped secrets (an API key the app presents, say) ride along.
	for _, r := range refs {
		cs.SecretRefs = append(cs.SecretRefs, secretRef{Name: r.Name, EnvVar: r.EnvVar})
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

// llmShmSizeMB is the shared-memory segment the inference runtimes need for
// tensor-parallel loading. The Docker default (64 MB) makes model load fail
// with an unhelpful error.
const llmShmSizeMB = 8192

// gpuRequest normalizes the device count. 0 (unset) means every GPU on the
// host — a dedicated inference box is the overwhelmingly common case, and
// asking for one device on a 4-GPU machine would waste three of them silently.
func gpuRequest(n int) int {
	if n <= 0 {
		return -1 // all
	}
	return n
}
