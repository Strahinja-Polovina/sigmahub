package store

// The digest of the canonical catalog source, shared by the generator and by
// both sides' staleness guards.
//
// The generated TypeScript embeds it, the Go staleness test recomputes it, and
// web/src/lib/hosting.test.ts recomputes it too — which is what lets the WEB
// suite notice a control-plane edit that never went through the generator.
// Hashing the source file rather than parsing it is deliberate: the previous
// mechanism regex-parsed this package's Go source from vitest, so a gofmt run
// or a comment inside a map literal could silently make the guard match a
// subset and pass.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// CatalogSourceFiles are every input the generated TypeScript is rendered
// from, relative to this package's directory. Both the generator (run by
// go:generate, cwd = this package) and the package's own tests resolve them
// from there. One entry deliberately points OUTSIDE the package — the sizing
// constants live in cp/internal/hf, and what matters is that everything the
// output embeds is hashed, not which package it came from. The digest names
// each file by its base name, so the web-side recomputation must do the same.
//
// All NINE, not just the catalog: the generated module also embeds the billing
// constants from billing.go, the cluster exclusion list from clusters.go, the
// engine catalogs from db_engines.go and s3_engines.go, and every literal the
// renderer itself writes. With only the catalog hashed, changing
// BillingCurrency from "EUR" to "USD" left the checked-in TypeScript saying EUR
// and the whole web suite green — the web-side guard is the one that has to
// notice a control-plane-only edit, so it has to cover everything that reaches
// the output.
//
// The engine files joined when demo mode stopped keeping a second copy of the
// engine table. Every value in it disagreed with this package: the demo
// advertised postgres:17-alpine against a control plane pinned to 16.6, and
// minio/minio:latest against a product whose agent policy refuses a floating
// tag outright. Left out of this list, the next version bump here would ship
// the same disagreement again with the web suite green.
//
// clusters.go is the awkward one and is here deliberately. It is a whole store
// file rather than a catalog table, so it changes for reasons that have nothing
// to do with the dashboard — a new query, a fixed scan — and each of those now
// moves the digest and asks for a regenerate that changes no rendered byte. That
// is the cost of the alternative being silent: leave it out and an edit to
// clusterExcludedKinds is invisible to the web suite, which is precisely the
// drift the generated list was added to remove. A stale-catalog failure names
// the command that fixes it; a demo mode offering a cluster for a Postgres does
// not announce itself at all. The engine catalogs are deliberately NOT that:
// they live in files of their own (the S3 one was split out of s3.go for this),
// so a query edit beside them cannot cost a regenerate.
//
// alerts_store.go is here for the same reason as clusters.go, and at the same
// cost. The alert event vocabulary (AlertEvents) is served to the dashboard as
// a plain string list, and the dashboard's label map enumerated a SUBSET of it:
// payment_failed had no label, so the rules editor rendered a raw
// "payment_failed" chip beside seven sentence-case ones and nothing anywhere
// failed (SIGMA-274). Rendering the vocabulary as a union makes the label map a
// total Record, so the omission is a tsc error at the point of the omission.
// The cost is that an unrelated edit in this file — a new query, a fixed scan —
// moves the digest and asks for a regenerate that changes no rendered byte;
// that failure names the command that fixes it, whereas an unlabelled event
// does not announce itself at all.
//
// llm_engines.go is here on the same terms (SIGMA-278). The wizard kept its own
// two-entry copy of the runtime catalog, so renaming or replacing the default
// runtime here left it sending engine "vllm" for every model whose card did not
// resolve, and provisionLLMTx answered with a 422 at the end of the LLM wizard.
// The engine files were split out of their query files precisely so a query
// edit could not cost a regenerate; this one is not split, because its catalog
// and its provisioning live in one file, and the drift is worth the cost.
//
// ../hf/sizing.go carries the VRAM formula's two constants and the bands
// FormatVRAM renders with (SIGMA-279). Demo mode records what the control plane
// would have answered for each model card; those figures were evaluated by hand
// once, and the demo asserted them against themselves — so a change to
// UtilizationCap or KVActivationFactor left every suite green while the demo
// quoted VRAM figures the product no longer produces, to exactly the people
// sizing a GPU purchase from them.
var CatalogSourceFiles = []string{
	"server_catalog.go",    // the canonical catalog: types, matrix, requirements
	"server_catalog_ts.go", // the renderer — its literals are output too
	"billing.go",           // unit price, free tier, currency
	"clusters.go",          // the kinds a cluster refuses
	"db_engines.go",        // database images, URL shapes, the mesh port base
	"s3_engines.go",        // object-storage images and endpoint shapes
	"alerts_store.go",      // the alert event vocabulary the rules editor labels
	"llm_engines.go",       // the inference runtimes and which one is the default
	"../hf/sizing.go",      // the VRAM formula's constants and its rendering bands
}

// CatalogSourceDigest returns the hex sha256 over the given sources, in the
// order given. Hashing rather than parsing is deliberate: the previous
// mechanism regex-parsed this package's Go source from vitest, so a gofmt run
// or a comment inside a map literal could silently make the guard match a
// subset and pass.
func CatalogSourceDigest(paths ...string) (string, error) {
	h := sha256.New()
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		// Length-prefixed so two files cannot be concatenated into the same
		// digest by moving a byte across the boundary between them.
		fmt.Fprintf(h, "%s:%d\n", filepath.Base(p), len(src))
		h.Write(src)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
