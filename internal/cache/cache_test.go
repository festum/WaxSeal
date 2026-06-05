package cache

import (
	"testing"
	"time"
)

func TestGetSetHit(t *testing.T) {
	c := New[string](8)
	c.Set("k", "v", time.Time{})
	if got, ok := c.Get("k"); !ok || got != "v" {
		t.Fatalf("get = %q,%v", got, ok)
	}
	if _, ok := c.Get("missing"); ok {
		t.Fatal("missing key should miss")
	}
}

func TestExpiryEvicts(t *testing.T) {
	clock := time.Now()
	c := New[int](8)
	c.now = func() time.Time { return clock }
	c.Set("k", 1, clock.Add(time.Minute))

	if _, ok := c.Get("k"); !ok {
		t.Fatal("should hit before expiry")
	}
	clock = clock.Add(2 * time.Minute)
	if _, ok := c.Get("k"); ok {
		t.Fatal("should miss after expiry")
	}
	if c.Len() != 0 {
		t.Fatalf("expired entry not deleted; len=%d", c.Len())
	}
}

func TestEvictionPrefersExpiredThenSoonest(t *testing.T) {
	clock := time.Now()
	c := New[string](2)
	c.now = func() time.Time { return clock }

	c.Set("expired", "x", clock.Add(time.Second))
	c.Set("soon", "s", clock.Add(time.Hour))
	clock = clock.Add(2 * time.Second) // "expired" is now stale

	// Inserting a third entry must evict the expired one, not "soon".
	c.Set("fresh", "f", clock.Add(2*time.Hour))
	if _, ok := c.Get("soon"); !ok {
		t.Fatal("evicted a live entry while an expired one existed")
	}
	if _, ok := c.Get("fresh"); !ok {
		t.Fatal("new entry missing")
	}

	// Fill again so both live; the next insert evicts the soonest-to-expire.
	c.Set("later", "l", clock.Add(3*time.Hour)) // evicts "soon" (soonest)
	if _, ok := c.Get("soon"); ok {
		t.Fatal("soonest-to-expire should have been evicted")
	}
}

func TestDeleteAndPurge(t *testing.T) {
	c := New[int](8)
	c.Set("a", 1, time.Time{})
	c.Set("b", 2, time.Time{})
	c.Delete("a")
	if _, ok := c.Get("a"); ok {
		t.Fatal("deleted key still present")
	}
	c.Purge()
	if c.Len() != 0 {
		t.Fatalf("purge left %d entries", c.Len())
	}
}
