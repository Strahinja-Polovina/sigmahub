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
// from there.
//
// All THREE, not just the catalog: the generated module also embeds the billing
// constants from billing.go and every literal the renderer itself writes. With
// only the catalog hashed, changing BillingCurrency from "EUR" to "USD" left the
// checked-in TypeScript saying EUR and the whole web suite green — the web-side
// guard is the one that has to notice a control-plane-only edit, so it has to
// cover everything that reaches the output.
var CatalogSourceFiles = []string{
	"server_catalog.go",    // the canonical catalog: types, matrix, requirements
	"server_catalog_ts.go", // the renderer — its literals are output too
	"billing.go",           // unit price, free tier, currency
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
