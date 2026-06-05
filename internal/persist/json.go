package persist

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// jsonStore is the opt-in single-process backend: one JSON file rewritten
// atomically (temp + rename, 0600) on each mutation. It does no cross-process
// locking, so bbolt remains the default when multiple processes may share a
// directory.
type jsonStore struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
	data fileData
}

type fileData struct {
	Fingerprint string               `json:"fingerprint"`
	Tokens      map[string]tokenBlob `json:"tokens"`
	Breakers    map[string]time.Time `json:"breakers"`
}

func openJSON(path, fp string, now func() time.Time) (Store, error) {
	s := &jsonStore{path: path, now: now, data: fileData{
		Fingerprint: fp,
		Tokens:      map[string]tokenBlob{},
		Breakers:    map[string]time.Time{},
	}}
	switch raw, err := os.ReadFile(path); {
	case err == nil:
		var existing fileData
		// A parse failure or fingerprint mismatch starts with an empty store;
		// only a valid, matching file is adopted.
		if json.Unmarshal(raw, &existing) == nil && existing.Fingerprint == fp {
			if existing.Tokens != nil {
				s.data.Tokens = existing.Tokens
			}
			if existing.Breakers != nil {
				s.data.Breakers = existing.Breakers
			}
		} else if err := s.flush(); err != nil {
			return nil, err
		}
	case os.IsNotExist(err):
		if err := s.flush(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("persist: read json %s: %w", path, err)
	}
	return s, nil
}

func (s *jsonStore) LoadTokens() (map[string]TokenRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make(map[string]TokenRecord, len(s.data.Tokens))
	for k, rec := range s.data.Tokens {
		if expired(rec.ExpiresAt, now) {
			continue
		}
		out[k] = TokenRecord{Data: rec.Data, ExpiresAt: rec.ExpiresAt}
	}
	return out, nil
}

func (s *jsonStore) SaveToken(key string, rec TokenRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Tokens[key] = tokenBlob{Data: rec.Data, ExpiresAt: rec.ExpiresAt}
	return s.flush()
}

func (s *jsonStore) DeleteToken(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Tokens, key)
	return s.flush()
}

func (s *jsonStore) PurgeTokens() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Tokens = map[string]tokenBlob{}
	return s.flush()
}

func (s *jsonStore) LoadBreakers() (map[string]time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make(map[string]time.Time, len(s.data.Breakers))
	for k, t := range s.data.Breakers {
		if t.After(now) {
			out[k] = t
		}
	}
	return out, nil
}

func (s *jsonStore) SaveBreaker(key string, openUntil time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if openUntil.IsZero() || !openUntil.After(s.now()) {
		delete(s.data.Breakers, key)
	} else {
		s.data.Breakers[key] = openUntil
	}
	return s.flush()
}

func (s *jsonStore) Close() error { return nil }

// flush atomically rewrites the file: write a sibling temp 0600, then rename
// over the target so a crash mid-write never leaves a truncated store.
func (s *jsonStore) flush() error {
	raw, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".store-*.tmp")
	if err != nil {
		return fmt.Errorf("persist: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}
