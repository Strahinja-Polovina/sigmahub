package dsd

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	doc := Document{Version: 3, OrgID: "org_1", ServerID: "srv_1", Ops: []Op{{ID: "a", Kind: "resource.sync"}}}
	sig, err := Sign(priv, doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, Signed{Document: doc, Signature: sig}); err != nil {
		t.Fatalf("verify valid: %v", err)
	}

	// Tampering the document must fail verification.
	tampered := doc
	tampered.Version = 4
	if err := Verify(pub, Signed{Document: tampered, Signature: sig}); err == nil {
		t.Fatal("verify accepted a tampered document")
	}

	// A different key must fail.
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := Verify(otherPub, Signed{Document: doc, Signature: sig}); err == nil {
		t.Fatal("verify accepted a foreign key")
	}
}

func ids(ops []Op) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = op.ID
	}
	return out
}

func TestTopoOrder(t *testing.T) {
	t.Run("dependency precedes dependent", func(t *testing.T) {
		ops := []Op{
			{ID: "c", DependsOn: []string{"b"}},
			{ID: "b", DependsOn: []string{"a"}},
			{ID: "a"},
		}
		ordered, unresolved := TopoOrder(ops)
		if len(unresolved) != 0 {
			t.Fatalf("unexpected unresolved: %v", unresolved)
		}
		pos := map[string]int{}
		for i, op := range ordered {
			pos[op.ID] = i
		}
		if !(pos["a"] < pos["b"] && pos["b"] < pos["c"]) {
			t.Fatalf("bad order: %v", ids(ordered))
		}
	})

	t.Run("cycle marked unresolved", func(t *testing.T) {
		ops := []Op{{ID: "a", DependsOn: []string{"b"}}, {ID: "b", DependsOn: []string{"a"}}}
		ordered, unresolved := TopoOrder(ops)
		if len(ordered) != 0 || len(unresolved) != 2 {
			t.Fatalf("cycle: ordered=%v unresolved=%v", ids(ordered), unresolved)
		}
	})

	t.Run("missing dependency cascades to dependents", func(t *testing.T) {
		ops := []Op{{ID: "a", DependsOn: []string{"ghost"}}, {ID: "b", DependsOn: []string{"a"}}, {ID: "c"}}
		ordered, unresolved := TopoOrder(ops)
		if len(ordered) != 1 || ordered[0].ID != "c" {
			t.Fatalf("expected only c ordered, got %v", ids(ordered))
		}
		if len(unresolved) != 2 {
			t.Fatalf("expected a and b unresolved, got %v", unresolved)
		}
	})
}
