package dsd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestVerifyRejectsTamperAndForeignKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	doc := Document{Version: 2, OrgID: "o", ServerID: "s", Ops: []Op{{ID: "a", Kind: "resource.sync", Spec: json.RawMessage(`{"k":1}`)}}}
	b, _ := json.Marshal(doc)
	sig := ed25519.Sign(priv, b)

	if err := Verify(pubB64, Signed{Document: doc, Signature: sig}); err != nil {
		t.Fatalf("valid doc rejected: %v", err)
	}

	// Tamper the version.
	bad := doc
	bad.Version = 99
	if err := Verify(pubB64, Signed{Document: bad, Signature: sig}); err == nil {
		t.Fatal("tampered document accepted")
	}

	// Foreign key.
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := Verify(base64.StdEncoding.EncodeToString(otherPub), Signed{Document: doc, Signature: sig}); err == nil {
		t.Fatal("foreign key accepted")
	}

	// Garbage signature.
	if err := Verify(pubB64, Signed{Document: doc, Signature: []byte("nope")}); err == nil {
		t.Fatal("garbage signature accepted")
	}
}

func TestTopoOrderMatchesControlPlane(t *testing.T) {
	ops := []Op{
		{ID: "c", DependsOn: []string{"a", "b"}},
		{ID: "b", DependsOn: []string{"a"}},
		{ID: "a"},
	}
	ordered, unresolved := TopoOrder(ops)
	if len(unresolved) != 0 {
		t.Fatalf("unresolved: %v", unresolved)
	}
	pos := map[string]int{}
	for i, op := range ordered {
		pos[op.ID] = i
	}
	if !(pos["a"] < pos["b"] && pos["b"] < pos["c"]) {
		t.Fatalf("bad order")
	}
}
