package container

import (
	"encoding/json"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

// desiredBucket holds the last-applied desired ContainerSpec per container name.
// The reconcile loop reads it to re-converge drift WITHOUT contacting the
// control plane, so managed workloads survive a CP outage.
var desiredBucket = []byte("desired")

// Store persists the agent's desired container set in its own bbolt file
// (containers.db, 0600) alongside the DSD journal.
type Store struct {
	db *bolt.DB
}

// OpenStore opens (creating if needed) the desired-state store in dataDir.
func OpenStore(dataDir string) (*Store, error) {
	db, err := bolt.Open(filepath.Join(dataDir, "containers.db"), 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(desiredBucket)
		return e
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// PutDesired records the desired spec for a container name.
func (s *Store) PutDesired(name string, spec ContainerSpec) error {
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(desiredBucket).Put([]byte(name), b)
	})
}

// DeleteDesired drops a container name from the desired set.
func (s *Store) DeleteDesired(name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(desiredBucket).Delete([]byte(name))
	})
}

// AllDesired returns every persisted desired container spec.
func (s *Store) AllDesired() (map[string]ContainerSpec, error) {
	out := map[string]ContainerSpec{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(desiredBucket).ForEach(func(k, v []byte) error {
			var spec ContainerSpec
			if err := json.Unmarshal(v, &spec); err != nil {
				return err
			}
			out[string(k)] = spec
			return nil
		})
	})
	return out, err
}
