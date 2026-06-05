package persist

import "time"

// nopStore is the memory-only fallback: persistence disabled, or a configured
// store that failed to open. Every method is a no-op so call sites need no
// nil-checks and behave exactly like the pre-persistence memory-only path.
type nopStore struct{}

// Nop returns a Store that persists nothing.
func Nop() Store { return nopStore{} }

func (nopStore) LoadTokens() (map[string]TokenRecord, error) { return nil, nil }
func (nopStore) SaveToken(string, TokenRecord) error         { return nil }
func (nopStore) DeleteToken(string) error                    { return nil }
func (nopStore) PurgeTokens() error                          { return nil }
func (nopStore) LoadBreakers() (map[string]time.Time, error) { return nil, nil }
func (nopStore) SaveBreaker(string, time.Time) error         { return nil }
func (nopStore) Close() error                                { return nil }
