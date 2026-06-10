package db

import (
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	BucketUsers  = []byte("users")
	BucketTokens = []byte("tokens")
	BucketNodes  = []byte("nodes")
	BucketConfig = []byte("config")
)

// Manager wraps the bbolt database and provides helper methods
type Manager struct {
	db *bolt.DB
}

// NewManager opens the bbolt database at dbPath and initializes required buckets
func NewManager(dbPath string) (*Manager, error) {
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("failed to open bolt database at %s: %w", dbPath, err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		buckets := [][]byte{BucketUsers, BucketTokens, BucketNodes, BucketConfig}
		for _, b := range buckets {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return fmt.Errorf("failed to create bucket %s: %w", b, err)
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Manager{db: db}, nil
}

// Close closes the underlying bolt database
func (m *Manager) Close() error {
	if m.db != nil {
		return m.db.Close()
	}
	return nil
}
