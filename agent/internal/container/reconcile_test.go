package container

import "testing"

// GC must never reap a peer's container. See ownedByAnotherServer for what this
// cost before the owner label existed: on a shared Docker daemon each agent
// deleted its peers' containers as orphans, so a multi-server deploy could not
// converge. The fleet e2e is the end-to-end proof; this pins the rule itself.
func TestOwnedByAnotherServer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		self   string
		want   bool
	}{
		{"a peer's container is left alone", map[string]string{LabelServerID: "srv_peer"}, "srv_mine", true},
		{"our own is ours to reap", map[string]string{LabelServerID: "srv_mine"}, "srv_mine", false},
		// An older build of this agent stamped no owner. Refusing to reap those
		// would leak every pre-upgrade container forever.
		{"an unlabelled container is ours", map[string]string{LabelManaged: "true"}, "srv_mine", false},
		// Before registration the agent has no id, so it cannot claim ownership
		// of anything — and must not start skipping its own cleanup.
		{"an agent with no id yet still reaps", map[string]string{LabelServerID: "srv_peer"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownedByAnotherServer(tc.labels, tc.self); got != tc.want {
				t.Fatalf("ownedByAnotherServer(%v, %q) = %v, want %v", tc.labels, tc.self, got, tc.want)
			}
		})
	}
}
