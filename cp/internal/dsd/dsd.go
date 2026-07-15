// Package dsd defines the Desired-State Document: the signed, versioned,
// per-server instruction set the reconciler renders and the agent applies.
// A DSD is an ordered list of typed ops carrying explicit intra-DSD
// dependencies; the op *kind* vocabulary is the single enforcement point for
// the "no generic run-shell" invariant (architecture §3 invariant c).
package dsd

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Op is one typed operation in a DSD. Kind is drawn from the registry; P1-2
// ships only the stub "resource.sync" — P1-3 registers container ops behind
// the same registry so shell-escape can never be added ad hoc.
type Op struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	DependsOn []string        `json:"dependsOn,omitempty"`
	Spec      json.RawMessage `json:"spec,omitempty"`
}

// Document is the desired state for one server at a monotonic version.
type Document struct {
	Version  int64  `json:"version"`
	OrgID    string `json:"orgId"`
	ServerID string `json:"serverId"`
	Ops      []Op   `json:"ops"`
}

// Signed wraps a document with its detached Ed25519 signature over the
// canonical JSON encoding.
type Signed struct {
	Document  Document `json:"document"`
	Signature []byte   `json:"signature"`
}

// canonical returns the deterministic bytes that are signed and verified.
// encoding/json sorts struct fields by declaration and map keys lexically, so
// as long as both sides marshal the same Document value the bytes match.
func canonical(doc Document) ([]byte, error) {
	return json.Marshal(doc)
}

// Sign produces a detached signature over the document.
func Sign(priv ed25519.PrivateKey, doc Document) ([]byte, error) {
	b, err := canonical(doc)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(priv, b), nil
}

// Verify checks a signed document against the pinned public key.
func Verify(pub ed25519.PublicKey, s Signed) error {
	b, err := canonical(s.Document)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, b, s.Signature) {
		return fmt.Errorf("dsd: signature verification failed")
	}
	return nil
}

// SpecHash is a stable digest of an op spec, used by the reconciler to decide
// whether the desired state actually changed (and a new version is due).
func SpecHash(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// TopoOrder returns the ops in dependency order (a dependency precedes its
// dependents) and the set of op ids that could not be ordered because they sit
// on a dependency cycle or reference a missing dependency. Callers treat
// unorderable ops as failed. Deterministic: ties break on input order.
func TopoOrder(ops []Op) (ordered []Op, unresolved []string) {
	byID := make(map[string]Op, len(ops))
	for _, op := range ops {
		byID[op.ID] = op
	}
	state := make(map[string]int) // 0=unseen,1=visiting,2=done
	var order []Op
	bad := make(map[string]bool)

	var visit func(id string) bool
	visit = func(id string) bool {
		op, ok := byID[id]
		if !ok {
			return false // missing dependency
		}
		switch state[id] {
		case 2:
			return !bad[id]
		case 1:
			return false // cycle
		}
		state[id] = 1
		for _, dep := range op.DependsOn {
			if !visit(dep) {
				state[id] = 2
				bad[id] = true
				return false
			}
		}
		state[id] = 2
		order = append(order, op)
		return true
	}

	for _, op := range ops {
		if state[op.ID] == 0 {
			_ = visit(op.ID)
		}
	}
	for _, op := range ops {
		if bad[op.ID] {
			unresolved = append(unresolved, op.ID)
		}
	}
	return order, unresolved
}
