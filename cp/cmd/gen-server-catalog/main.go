// Command gen-server-catalog renders the control plane's canonical server-type
// catalog into the dashboard's TypeScript module.
//
// Run by `go generate ./...` from cp/ — the //go:generate directive lives on
// cp/internal/store/server_catalog.go, so the working directory is that
// package and the default paths below are relative to it. A staleness test in
// package store re-renders and byte-compares, so forgetting to run this fails
// the CP build rather than shipping a dashboard that disagrees with the API.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func main() {
	srcDir := flag.String("src-dir", ".", "directory holding the catalog sources")
	out := flag.String("out", filepath.Join("..", "..", "..", "web", "src", "lib", "server-catalog.generated.ts"),
		"TypeScript module to write")
	flag.Parse()

	srcs := make([]string, 0, len(store.CatalogSourceFiles))
	for _, f := range store.CatalogSourceFiles {
		srcs = append(srcs, filepath.Join(*srcDir, f))
	}
	sha, err := store.CatalogSourceDigest(srcs...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-server-catalog: read catalog source:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, store.RenderTypeScript(sha), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen-server-catalog: write:", err)
		os.Exit(1)
	}
}
