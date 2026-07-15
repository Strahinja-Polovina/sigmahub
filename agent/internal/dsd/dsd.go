// Package dsd is the agent's view of the Desired-State Document: the same
// signed, versioned, ordered-typed-op shape the control plane renders, plus
// verification against the pinned CP signing key and dependency ordering.
package dsd

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

type Op struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	DependsOn []string        `json:"dependsOn,omitempty"`
	Spec      json.RawMessage `json:"spec,omitempty"`
}

type Document struct {
	Version  int64  `json:"version"`
	OrgID    string `json:"orgId"`
	ServerID string `json:"serverId"`
	Ops      []Op   `json:"ops"`
}

type Signed struct {
	Document  Document `json:"document"`
	Signature []byte   `json:"signature"`
}

// Verify checks the signed document against the base64-encoded pinned key.
func Verify(pubB64 string, s Signed) error {
	raw, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		return fmt.Errorf("dsd: bad pinned key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("dsd: pinned key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	b, err := json.Marshal(s.Document)
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(raw), b, s.Signature) {
		return fmt.Errorf("dsd: signature verification failed")
	}
	return nil
}

// TopoOrder returns ops in dependency order (dependency before dependent) and
// the ids that can't be ordered (cycle or missing dependency); callers treat
// those as failed. Mirrors the control-plane ordering exactly.
func TopoOrder(ops []Op) (ordered []Op, unresolved []string) {
	byID := make(map[string]Op, len(ops))
	for _, op := range ops {
		byID[op.ID] = op
	}
	state := make(map[string]int)
	bad := make(map[string]bool)
	var order []Op

	var visit func(id string) bool
	visit = func(id string) bool {
		op, ok := byID[id]
		if !ok {
			return false
		}
		switch state[id] {
		case 2:
			return !bad[id]
		case 1:
			return false
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
