// Package apply owns the agent's DSD application: an on-disk journal (so a
// crash mid-DSD resumes without re-running completed ops) and the
// dependency-ordered apply loop over the typed-op registry.
package apply

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketMeta    = []byte("meta")    // last-applied version + doc
	bucketJournal = []byte("journal") // per-(version,opID) result
)

// OpResult is the recorded outcome of one op.
type OpResult struct {
	Version int64     `json:"version"`
	OpID    string    `json:"opId"`
	State   string    `json:"state"` // "applied" | "failed" | "skipped"
	Err     string    `json:"err,omitempty"`
	At      time.Time `json:"at"`
}

// Journal is a bbolt-backed record of applied DSDs and per-op results.
type Journal struct {
	db *bolt.DB
}

// Open creates/opens the journal at dataDir/journal.db (0600 — it mirrors
// desired state, no secrets, but stays owner-only alongside the identity).
func Open(dataDir string) (*Journal, error) {
	db, err := bolt.Open(filepath.Join(dataDir, "journal.db"), 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(bucketMeta); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(bucketJournal)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Journal{db: db}, nil
}

func (j *Journal) Close() error { return j.db.Close() }

// LastAppliedVersion returns the highest fully-applied DSD version (0 if none).
func (j *Journal) LastAppliedVersion() (int64, error) {
	var v int64
	err := j.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta).Get([]byte("lastAppliedVersion"))
		if b != nil {
			return json.Unmarshal(b, &v)
		}
		return nil
	})
	return v, err
}

// SetLastAppliedVersion records a DSD as fully applied (all ops journaled).
func (j *Journal) SetLastAppliedVersion(v int64) error {
	return j.db.Update(func(tx *bolt.Tx) error {
		b, _ := json.Marshal(v)
		return tx.Bucket(bucketMeta).Put([]byte("lastAppliedVersion"), b)
	})
}

func key(version int64, opID string) []byte {
	return []byte(fmt.Sprintf("%020d:%s", version, opID))
}

// Record persists one op result.
func (j *Journal) Record(res OpResult) error {
	return j.db.Update(func(tx *bolt.Tx) error {
		b, err := json.Marshal(res)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketJournal).Put(key(res.Version, res.OpID), b)
	})
}

// Result returns the recorded result for (version, opID), or ok=false.
func (j *Journal) Result(version int64, opID string) (OpResult, bool, error) {
	var res OpResult
	found := false
	err := j.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketJournal).Get(key(version, opID))
		if b == nil {
			return nil
		}
		found = true
		return json.Unmarshal(b, &res)
	})
	return res, found, err
}
