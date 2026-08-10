package api

// What this control plane can actually be asked for (SIGMA-268).
//
// The dashboard builds its wizard out of the GENERATED catalog
// (web/src/lib/server-catalog.generated.ts), which is the full set of engines
// this codebase knows how to provision. The control plane it is talking to has
// its own, possibly smaller, view: CP_DB_ENGINES / CP_S3_ENGINES cut the list
// (a Postgres-only build is a supported, pre-agreed shape), and store.CreateResource
// refuses anything outside it with `database engine "x" is not enabled on this
// control plane`.
//
// Nothing published that second list, so the wizard could not know it. Both
// halves of the disagreement then failed in the same place and in the worst
// possible way: the operator picked an engine the dialog offered, filled in a
// name, chose a project, an environment and a server, pressed create, and got a
// 422 — after the dialog had closed. The engines are a property of the control
// plane, so the control plane is what has to say them out loud.
//
// Org-scoped and Developer-readable because the wizard is: this is not a
// secret (it is a shape of the deployment, visible in every refusal already),
// but it belongs behind the same token as everything else the dashboard reads,
// and routing it under /v1/orgs/{orgId} keeps it on the one authenticated
// surface rather than inventing a second unauthenticated one.

import (
	"net/http"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// capabilitiesResponse is deliberately a list of NAMES rather than a rendered
// catalog. The labels, hints and icons are the dashboard's, generated from this
// same Go catalog, so sending them again would be a second copy of the
// vocabulary SIGMA-216 spent its time deleting. What the dashboard cannot
// derive is which of them this deployment turned off.
type capabilitiesResponse struct {
	DBEngines []string `json:"dbEngines"`
	S3Engines []string `json:"s3Engines"`
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, capabilitiesResponse{
		DBEngines: enabledOrAll(s.dbEngines, store.DBEngineKinds()),
		S3Engines: enabledOrAll(s.s3Engines, store.S3EngineNames()),
	})
}

// enabledOrAll answers with the configured allowlist, or with the whole catalog
// when none was configured.
//
// The fallback matters more than it looks: config.FromEnv already resolves an
// unset CP_DB_ENGINES to the full catalog, so a server built from a real Config
// never takes this branch. A Server built WITHOUT those options — every handler
// unit test, and any future embedding — would otherwise publish "no engines are
// enabled here", and a wizard that believes it would offer nothing at all. An
// absent allowlist means "not restricted", exactly as it does in the store.
func enabledOrAll(enabled, all []string) []string {
	if len(enabled) == 0 {
		return all
	}
	// Copied so a caller cannot mutate the server's configuration through the
	// slice it was handed, and ordered by the catalog rather than by the
	// operator's typing, so two control planes with the same engines enabled
	// answer identically.
	out := make([]string, 0, len(enabled))
	for _, name := range all {
		for _, e := range enabled {
			if e == name {
				out = append(out, name)
				break
			}
		}
	}
	return out
}
