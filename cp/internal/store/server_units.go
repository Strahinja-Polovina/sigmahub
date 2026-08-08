package store

// Billing units (P2-4b). A server is billed as a number of UNITS whose weight
// tracks how expensive that server is to manage, not what the hardware costs —
// customers always bring their own infrastructure and we never mark it up.
//
// The weights live here and nowhere else: the summary, the checkout quantity
// and the drift sweep all derive from this map (the sweep through the generated
// SQL CASE below), and a web-side test asserts the dashboard read model agrees.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// DefaultServerUnitWeight is what an unknown/legacy server type costs. Unknown
// types must bill as an ordinary server rather than as zero: a typo in a type
// string should never silently make a server free.
const DefaultServerUnitWeight = 1

// serverUnitWeights maps a server type to its billing weight.
//
//   - general/database/storage: an ordinary Docker host. One unit — today's
//     price, unchanged.
//   - k8s: cluster lifecycle, upgrades and networking on top of the host.
//   - gpu: drivers, CUDA, the model runtime and metering — the most expensive
//     management profile we run, and the one that replaces an MLOps hire.
var serverUnitWeights = map[string]int{
	"general":  1,
	"database": 1,
	"storage":  1,
	"k8s":      2,
	"gpu":      4,
}

// serverTypePattern is what a weight key may contain. Enforced at init so
// unitWeightSQL can inline the keys as SQL literals by construction rather than
// by trust.
var serverTypePattern = regexp.MustCompile(`^[a-z0-9_]+$`)

func init() {
	for t := range serverUnitWeights {
		if !serverTypePattern.MatchString(t) {
			panic("store: invalid server type in serverUnitWeights: " + t)
		}
	}
}

// ServerUnitWeight returns the billing weight for a server type.
func ServerUnitWeight(serverType string) int {
	if w, ok := serverUnitWeights[serverType]; ok {
		return w
	}
	return DefaultServerUnitWeight
}

// ServerUnitWeights returns a copy of the weight table (for the API to publish
// to the dashboard, so the breakdown never hardcodes its own copy).
func ServerUnitWeights() map[string]int {
	out := make(map[string]int, len(serverUnitWeights))
	for t, w := range serverUnitWeights {
		out[t] = w
	}
	return out
}

// unitWeightSQL renders the weight table as a SQL CASE over a server-type
// column. Some billing arithmetic (the drift sweep's correlated subqueries) has
// to happen in the database; generating the expression from the same map keeps
// one source of truth instead of a hand-copied second one that can drift.
func unitWeightSQL(col string) string {
	types := make([]string, 0, len(serverUnitWeights))
	for t := range serverUnitWeights {
		types = append(types, t)
	}
	sort.Strings(types) // deterministic query text (statement-cache friendly)

	var b strings.Builder
	b.WriteString("CASE " + col)
	for _, t := range types {
		// Safe by construction: init() rejects any key outside [a-z0-9_].
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", t, serverUnitWeights[t])
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
