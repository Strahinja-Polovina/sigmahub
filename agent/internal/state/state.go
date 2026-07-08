// Package state persists the agent's identity so restarts don't re-register.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type State struct {
	ServerID   string `json:"serverId"`
	AgentToken string `json:"agentToken"`
}

func path(dataDir string) string { return filepath.Join(dataDir, "sigmad.json") }

// Load returns (state, true) when a valid identity exists.
func Load(dataDir string) (State, bool, error) {
	b, err := os.ReadFile(path(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, false, fmt.Errorf("corrupt state file %s: %w", path(dataDir), err)
	}
	if st.ServerID == "" || st.AgentToken == "" {
		return State{}, false, nil
	}
	return st, true, nil
}

// Save writes the identity with owner-only permissions (it embeds the agent
// credential).
func Save(dataDir string, st State) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path(dataDir))
}
