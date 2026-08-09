package store

// The object-storage engine catalog: the ONE place an S3 engine's image, API
// port and endpoint shape are written down.
//
// It sits in its own file, apart from the s3 resource store (s3.go), for the
// same reason db_engines.go does: every file that reaches the dashboard's
// generated catalog is hashed into the catalog digest, so a file holding both
// the catalog and a page of queries would make each unrelated query edit demand
// a regenerate that changes no rendered byte.

import (
	"strconv"
	"strings"
)

// DefaultS3Engine is what an s3 resource provisions when its spec names no
// engine. MinIO stays the default; SeaweedFS is opt-in (the Apache-2.0 hedge
// against MinIO's AGPL license, per P2-2).
const DefaultS3Engine = "minio"

// S3EngineDef pins one object-storage engine the CP can provision. Both engines
// take their root credentials purely through environment variables so the agent
// stays engine-agnostic: the access key rides plain env, the secret rides an
// env-mode secret reference. (SeaweedFS also supports a JSON config file, but
// that reloads only on SIGHUP — which would need engine-specific agent code —
// so env-var admin credentials are the deliberate choice, pinned by digest to
// freeze the behavior against upstream env-cred regressions.)
type S3EngineDef struct {
	Engine string
	// Image is the pinned release; the agent's image policy refuses floating
	// tags. A digest (@sha256:) is the ideal, immutable form.
	Image string
	// APIPort is the in-container S3 API port.
	APIPort int
	// DataMount is where the data volume mounts.
	DataMount string
	// AccessKeyEnv is the plain (non-secret) env var carrying the access key.
	AccessKeyEnv string
	// StaticEnv is the non-credential engine environment (console toggles etc).
	StaticEnv map[string]string
	// SecretEnvNames are env vars carrying the generated secret key; the DSD
	// renders them as secret REFERENCES only.
	SecretEnvNames []string
	// Cmd is the container command that starts the S3 API.
	Cmd []string
	// EndpointTemplate is the shape of the URL an S3 client dials: {host} and
	// {port}, substituted once (EndpointURL). Templated for the same reason the
	// database URLs are — the dashboard renders it too, in demo mode, where
	// there is no control plane to ask.
	EndpointTemplate string
}

// s3Engines is a SLICE rather than a map because its order is rendered into a
// checked-in file: a map range would reshuffle the generated TypeScript on most
// runs, so `go generate` would produce a different file each time and the
// staleness test would fail on commits that changed nothing. The default engine
// is written first, which is also the order the dashboard offers them in.
var s3Engines = []S3EngineDef{
	// MinIO: the P2-1 engine. Console disabled — the dashboard is the UI, and
	// one less exposed surface is one less thing to harden.
	{
		Engine:           "minio",
		Image:            "minio/minio:RELEASE.2025-04-22T22-12-26Z",
		APIPort:          9000,
		DataMount:        "/data",
		AccessKeyEnv:     "MINIO_ROOT_USER",
		StaticEnv:        map[string]string{"MINIO_BROWSER": "off"},
		SecretEnvNames:   []string{"MINIO_ROOT_PASSWORD"},
		Cmd:              []string{"server", "/data", "--address", ":9000"},
		EndpointTemplate: "http://{host}:{port}",
	},
	// SeaweedFS: all-in-one `weed server -s3`, S3 gateway on 8333. Pinned to 3.94
	// by digest — the release where AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY seed
	// the admin S3 identity (regressed in 3.97, upstream #7311); the digest
	// freezes that behavior. Only the S3 port is published on the mesh; the
	// master/volume/filer ports stay container-internal.
	{
		Engine:           "seaweedfs",
		Image:            "chrislusf/seaweedfs:3.94@sha256:d8ab8284bf4fd221e3cbf25f114f2f317c5bc942b1df032c43cc9d7bfe9bb1c6",
		APIPort:          8333,
		DataMount:        "/data",
		AccessKeyEnv:     "AWS_ACCESS_KEY_ID",
		StaticEnv:        map[string]string{},
		SecretEnvNames:   []string{"AWS_SECRET_ACCESS_KEY"},
		Cmd:              []string{"server", "-dir=/data", "-s3", "-s3.port=8333"},
		EndpointTemplate: "http://{host}:{port}",
	},
}

// s3EngineByName indexes the slice above; built at package load so a duplicate
// or unnamed engine is a startup failure rather than a lookup that silently
// resolves to whichever entry came first.
var s3EngineByName map[string]S3EngineDef

func init() {
	s3EngineByName = make(map[string]S3EngineDef, len(s3Engines))
	for _, def := range s3Engines {
		if def.Engine == "" {
			panic("store: unnamed s3 engine in the catalog")
		}
		if _, dup := s3EngineByName[def.Engine]; dup {
			panic("store: duplicate s3 engine in the catalog: " + def.Engine)
		}
		if def.Image == "" || def.EndpointTemplate == "" {
			panic("store: s3 engine " + def.Engine + " has no image or no endpoint template")
		}
		s3EngineByName[def.Engine] = def
	}
	if _, ok := s3EngineByName[DefaultS3Engine]; !ok {
		panic("store: the default s3 engine " + DefaultS3Engine + " is not in the catalog")
	}
}

// S3EngineByName returns the engine definition for an engine name (minio,
// seaweedfs). ok=false for an unknown engine.
func S3EngineByName(engine string) (S3EngineDef, bool) {
	def, ok := s3EngineByName[engine]
	return def, ok
}

// IsS3Engine reports whether the name is a known object-storage engine.
func IsS3Engine(engine string) bool {
	_, ok := s3EngineByName[engine]
	return ok
}

// S3EngineCatalog returns every engine definition in catalog order — what the
// TypeScript generator renders.
func S3EngineCatalog() []S3EngineDef {
	out := make([]S3EngineDef, len(s3Engines))
	copy(out, s3Engines)
	return out
}

// S3EngineNames lists the engine names in the same order. They are names and
// not resource kinds on purpose: `s3` is the kind, and which engine serves it
// is a per-resource choice under it (P2-2).
func S3EngineNames() []string {
	out := make([]string, 0, len(s3Engines))
	for _, def := range s3Engines {
		out = append(out, def.Engine)
	}
	return out
}

// PlainEnv is the non-secret engine environment: the access key under the
// engine's access-key env var, plus any static engine env.
func (d S3EngineDef) PlainEnv(accessKey string) map[string]string {
	env := map[string]string{d.AccessKeyEnv: accessKey}
	for k, v := range d.StaticEnv {
		env[k] = v
	}
	return env
}

// Command starts the S3 API on the fixed in-container port.
func (d S3EngineDef) Command() []string { return d.Cmd }

// EndpointURL renders the mesh endpoint S3 clients dial. The port is the
// ALLOCATED mesh port, never APIPort: the container's 9000 is published on
// whatever number the per-server allocator handed out. An empty host means the
// server has not finished mesh enrollment and there is no endpoint yet — an
// engine this catalog does not know renders nothing for the same reason.
func (d S3EngineDef) EndpointURL(host string, port int) string {
	if host == "" || d.EndpointTemplate == "" {
		return ""
	}
	return strings.NewReplacer(
		"{host}", host,
		"{port}", strconv.Itoa(port),
	).Replace(d.EndpointTemplate)
}
