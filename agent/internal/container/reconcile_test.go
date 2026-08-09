package container

import (
	"encoding/json"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// SIGMA-230. GC derives its keep-set from the document it is about to apply and
// runs BEFORE the ops, so a resource whose rollout op the control plane
// deliberately withheld — a dedicated build server still building, a Compose
// service gated on a remote dependency — reached GC represented only by its
// `sync:<id>` stub. The stub named no containers, so the live
// generation-suffixed container matched neither `want` nor a rollout group and
// was force-removed: the app went down for the whole build, not the swap window.
// The stub now states what to retain, and GC must honour it.
func TestGCRetainsGroupsTheDocumentHoldsBack(t *testing.T) {
	const self = "srv_web"
	stub := func(resourceID string, retain ...string) dsd.Op {
		spec, err := json.Marshal(resourceSyncSpec{ResourceID: resourceID, Retain: retain})
		if err != nil {
			t.Fatal(err)
		}
		return dsd.Op{ID: "sync:" + resourceID, Kind: KindResourceSync, Spec: spec}
	}
	live := func(name, resourceID, service string) ContainerState {
		return ContainerState{
			ID: "id-" + name, Name: name, Running: true,
			Labels: map[string]string{
				LabelManaged: "true", LabelResourceID: resourceID,
				LabelService: service, LabelServerID: self,
			},
		}
	}

	// The held-back single-container app: the document says nothing but "I still
	// want res_1's container", and the running generation must survive.
	doc := dsd.Document{Version: 7, ServerID: self, Ops: []dsd.Op{stub("res_1", "")}}
	want, protected := desiredNames(doc), protectedGroups(doc)
	if gcReap(live("sigmahub-res_1-9f1c", "res_1", ""), want, protected, self) {
		t.Fatal("GC reaped the live generation of a resource whose rollout the CP held back")
	}

	// A Compose app whose only locally-placed service is gated on a remote
	// dependency: the retained service survives, a service dropped from the
	// compose file (absent from the retain list) is still collected.
	doc = dsd.Document{Version: 8, ServerID: self, Ops: []dsd.Op{stub("res_2", "api")}}
	want, protected = desiredNames(doc), protectedGroups(doc)
	if gcReap(live("sigmahub-res_2-api-aa01", "res_2", "api"), want, protected, self) {
		t.Fatal("GC reaped a gated Compose service's live container")
	}
	if !gcReap(live("sigmahub-res_2-worker-aa01", "res_2", "worker"), want, protected, self) {
		t.Fatal("a service removed from the compose file must still be garbage-collected")
	}

	// Retention is opt-in and per resource: a stub with no retain list (a
	// resource that genuinely has nothing running) protects nothing, and a
	// resource with no representation in the document at all is still reaped.
	doc = dsd.Document{Version: 9, ServerID: self, Ops: []dsd.Op{stub("res_3")}}
	want, protected = desiredNames(doc), protectedGroups(doc)
	if !gcReap(live("sigmahub-res_3-bb02", "res_3", ""), want, protected, self) {
		t.Fatal("a stub with no retain list must not protect anything")
	}
	if !gcReap(live("sigmahub-res_deleted-cc03", "res_deleted", ""), want, protected, self) {
		t.Fatal("a deleted resource's container must still be garbage-collected")
	}
}

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
