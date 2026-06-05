package persist

import (
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Bucket names. meta holds the fingerprint; tokens and breakers hold the two
// persisted namespaces.
var (
	metaBucket     = []byte("meta")
	tokensBucket   = []byte("tokens")
	breakersBucket = []byte("breakers")
	fingerprintKey = []byte("fingerprint")
)

// bboltStore is the default backend. bbolt brings its own flock, so two
// processes sharing a directory are serialized rather than corrupting the file.
type bboltStore struct {
	db  *bolt.DB
	now func() time.Time
}

func openBolt(path, fp string, opts Options) (Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: opts.OpenTimeout})
	if err != nil {
		// A flock timeout (another process holds the file) or a slow mount lands
		// here; the caller logs and falls back to memory-only.
		return nil, fmt.Errorf("persist: open bbolt %s: %w", path, err)
	}
	s := &bboltStore{db: db, now: opts.now}
	if err := s.init(fp); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// init ensures the buckets exist and resets the store when its persisted
// fingerprint differs from the current one.
func (s *bboltStore) init(fp string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}
		stored := string(meta.Get(fingerprintKey))
		if stored != fp {
			for _, name := range [][]byte{tokensBucket, breakersBucket} {
				if tx.Bucket(name) != nil {
					if err := tx.DeleteBucket(name); err != nil {
						return err
					}
				}
			}
			if err := meta.Put(fingerprintKey, []byte(fp)); err != nil {
				return err
			}
		}
		if _, err := tx.CreateBucketIfNotExists(tokensBucket); err != nil {
			return err
		}
		_, err = tx.CreateBucketIfNotExists(breakersBucket)
		return err
	})
}

func (s *bboltStore) LoadTokens() (map[string]TokenRecord, error) {
	out := make(map[string]TokenRecord)
	now := s.now()
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(tokensBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var rec tokenBlob
			if err := json.Unmarshal(v, &rec); err != nil {
				return nil // skip an unreadable entry rather than fail the whole load
			}
			if expired(rec.ExpiresAt, now) {
				return nil
			}
			out[string(k)] = TokenRecord{Data: rec.Data, ExpiresAt: rec.ExpiresAt}
			return nil
		})
	})
	return out, err
}

func (s *bboltStore) SaveToken(key string, rec TokenRecord) error {
	v, err := json.Marshal(tokenBlob{Data: rec.Data, ExpiresAt: rec.ExpiresAt})
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(tokensBucket)
		if b == nil {
			return nil
		}
		return b.Put([]byte(key), v)
	})
}

func (s *bboltStore) DeleteToken(key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(tokensBucket)
		if b == nil {
			return nil
		}
		return b.Delete([]byte(key))
	})
}

func (s *bboltStore) PurgeTokens() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(tokensBucket) != nil {
			if err := tx.DeleteBucket(tokensBucket); err != nil {
				return err
			}
		}
		_, err := tx.CreateBucketIfNotExists(tokensBucket)
		return err
	})
}

func (s *bboltStore) LoadBreakers() (map[string]time.Time, error) {
	out := make(map[string]time.Time)
	now := s.now()
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(breakersBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			t, err := time.Parse(time.RFC3339Nano, string(v))
			if err != nil || !t.After(now) {
				return nil // skip unparseable or already-elapsed cooldowns
			}
			out[string(k)] = t
			return nil
		})
	})
	return out, err
}

func (s *bboltStore) SaveBreaker(key string, openUntil time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(breakersBucket)
		if b == nil {
			return nil
		}
		// A cleared or elapsed cooldown is removed rather than stored.
		if openUntil.IsZero() || !openUntil.After(s.now()) {
			return b.Delete([]byte(key))
		}
		return b.Put([]byte(key), []byte(openUntil.Format(time.RFC3339Nano)))
	})
}

func (s *bboltStore) Close() error { return s.db.Close() }
