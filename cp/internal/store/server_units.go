package store

// Billing units (P2-4b). A server is billed as a number of UNITS whose weight
// tracks how expensive that server is to manage, not what the hardware costs —
// customers always bring their own infrastructure and we never mark it up.
//
// The weight is a FIELD of the server type (server_catalog.go), not a table
// beside it: `vps` and `build` used to be missing from the separate weight map
// entirely and billed at the fallback weight by accident, because adding a
// server type and adding its price were two edits in two files and only the
// first one was obvious. The summary, the checkout quantity and the drift sweep
// all read the catalog (the sweep through the generated SQL CASE below).

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultServerUnitWeight is what an unknown/legacy server type costs. Unknown
// types must bill as an ordinary server rather than as zero: a typo in a type
// string should never silently make a server free.
const DefaultServerUnitWeight = 1

// serverTypePattern is what a server type may contain. Enforced when the
// catalog is indexed so unitWeightSQL can inline the types as SQL literals by
// construction rather than by trust.
var serverTypePattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// ServerUnitWeight returns the billing weight for a server type.
func ServerUnitWeight(serverType string) int {
	if spec, ok := ServerTypeSpecFor(serverType); ok {
		return spec.UnitWeight
	}
	return DefaultServerUnitWeight
}

// ServerUnitWeights returns the weight table (for the API to publish to the
// dashboard, so the breakdown never hardcodes its own copy).
func ServerUnitWeights() map[string]int {
	out := make(map[string]int, len(serverCatalog))
	for _, spec := range serverCatalog {
		out[spec.Type] = spec.UnitWeight
	}
	return out
}

// unitWeightSQL renders the weight table as a SQL CASE over a server-type
// column. Some billing arithmetic (the drift sweep's correlated subqueries) has
// to happen in the database; generating the expression from the catalog keeps
// one source of truth instead of a hand-copied second one that can drift.
func unitWeightSQL(col string) string {
	var b strings.Builder
	b.WriteString("CASE " + col)
	// Sorted, not catalog order: the query text must be stable across a
	// reordering of the catalog so the statement cache keeps hitting.
	for _, t := range sortedTypes() {
		// Safe by construction: the catalog rejects any type outside [a-z0-9_].
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", t, ServerUnitWeight(t))
	}
	fmt.Fprintf(&b, " ELSE %d END", DefaultServerUnitWeight)
	return b.String()
}

// ServerUnitLine is one row of the billing breakdown: how many servers of a
// type are connected and what they contribute in units.
type ServerUnitLine struct {
	Type   string `json:"type"`
	Count  int    `json:"count"`
	Weight int    `json:"weight"`
	Units  int    `json:"units"`
}
