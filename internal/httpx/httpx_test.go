package httpx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadBodyCapped(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		max     int64
		wantErr bool
	}{
		{"under", "hello", 10, false},
		{"exact", "hello", 5, false},
		{"over", "hello!", 5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadBodyCapped(strings.NewReader(tc.body), tc.max)
			if tc.wantErr {
				if !errors.Is(err, ErrBodyTooLarge) {
					t.Fatalf("want ErrBodyTooLarge, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if string(got) != tc.body {
				t.Fatalf("got %q want %q", got, tc.body)
			}
		})
	}
}

func TestDoRetriesThenSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Body must survive every retry (rewound via GetBody).
		b, _ := io.ReadAll(r.Body)
		if string(b) != "payload" {
			t.Errorf("attempt %d body = %q", atomic.LoadInt32(&hits), b)
		}
		if atomic.AddInt32(&hits, 1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := New(srv.Client())
	c.BaseDelay = time.Millisecond
	req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader([]byte("payload")))
	body, err := c.DoJSON(req, 1<<10)
	if err != nil {
		t.Fatalf("DoJSON: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("server hit %d times, want 3 (2 fail + 1 ok)", got)
	}
}

// A connection dropped mid-body (declared Content-Length not satisfied, then a
// short close) must be retried by DoJSON rather than surfaced as a failure. The
// request only succeeded at the header level.
func TestDoJSONRetriesOnMidBodyDrop(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Content-Length", "2048") // promise more than we write
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("truncated-body")) // then return with a short close
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("complete"))
	}))
	defer srv.Close()

	c := New(srv.Client())
	c.BaseDelay = time.Millisecond
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	body, err := c.DoJSON(req, 1<<20)
	if err != nil {
		t.Fatalf("DoJSON should retry a mid-body drop: %v", err)
	}
	if string(body) != "complete" {
		t.Fatalf("body = %q, want complete (the retried, intact response)", body)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("server hit %d times, want 2 (truncated then complete)", got)
	}
}

func TestDoGivesUpAfterMaxRetries(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.Client())
	c.BaseDelay = time.Millisecond
	c.MaxRetries = 2
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if _, err := c.DoJSON(req, 1<<10); err == nil {
		t.Fatal("want error after exhausting retries")
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("server hit %d times, want 3 (1 + 2 retries)", got)
	}
}

func TestDoHonorsRetryAfter(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.Client())
	c.MaxDelay = 5 * time.Second
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	start := time.Now()
	if _, err := c.DoJSON(req, 1<<10); err != nil {
		t.Fatalf("DoJSON: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("did not honor Retry-After: waited %v", elapsed)
	}
}

func TestDoNoRetryOnCanceledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.Client())
	c.BaseDelay = time.Hour // a retry would block ~forever; cancellation must win
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req = req.WithContext(ctx)

	done := make(chan error, 1)
	go func() { _, err := c.DoJSON(req, 1<<10); done <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want error on cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Do did not return promptly after cancel")
	}
}

func TestBreakerOpensAndResets(t *testing.T) {
	clock := time.Now()
	b := NewBreaker(3, 30*time.Second)
	b.now = func() time.Time { return clock }

	for i := 0; i < 2; i++ {
		b.RecordFailure()
		if _, err := b.Allow(); err != nil {
			t.Fatalf("breaker opened early after %d failures", i+1)
		}
	}
	b.RecordFailure() // third failure trips it
	rem, err := b.Allow()
	if !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("want ErrBreakerOpen, got %v", err)
	}
	if rem <= 0 {
		t.Fatalf("want positive cooldown, got %v", rem)
	}

	clock = clock.Add(31 * time.Second) // cooldown elapses
	if _, err := b.Allow(); err != nil {
		t.Fatalf("breaker should be closed after cooldown: %v", err)
	}

	b.RecordFailure()
	b.RecordSuccess() // success clears the streak
	for i := 0; i < 2; i++ {
		b.RecordFailure()
	}
	if _, err := b.Allow(); err != nil {
		t.Fatalf("streak not reset by success: %v", err)
	}
}

func TestBreakerPoisonRate(t *testing.T) {
	clock := time.Now()
	b := &Breaker{PoisonRate: 3, PoisonWindow: 60 * time.Second, now: func() time.Time { return clock }}

	b.RecordPoison()
	b.RecordPoison()
	if _, err := b.Allow(); err != nil {
		t.Fatal("opened before poison rate reached")
	}
	b.RecordPoison() // third poison within window trips it
	if _, err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("want ErrBreakerOpen from poison rate, got %v", err)
	}
}
