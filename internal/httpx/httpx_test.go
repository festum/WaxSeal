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

// errRoundTripper fails every attempt with a fixed retryable transport error.
type errRoundTripper struct {
	err   error
	calls *int32
}

func (rt errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	atomic.AddInt32(rt.calls, 1)
	return nil, rt.err
}

// When a retry's body rewind (GetBody) fails, the original transport error that
// triggered the retry must still surface rather than be masked by the prep error.
func TestDoJSONRewindFailurePreservesTransportError(t *testing.T) {
	transportErr := errors.New("connection reset")
	var calls int32
	c := New(&http.Client{Transport: errRoundTripper{err: transportErr, calls: &calls}})
	c.BaseDelay = time.Millisecond

	req, _ := http.NewRequest(http.MethodPost, "http://example.invalid/", strings.NewReader("payload"))
	req.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("getbody boom") }

	_, err := c.DoJSON(req, 1<<10)
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, transportErr) {
		t.Errorf("err = %v, want it to wrap the original transport error %v", err, transportErr)
	}
	// The retry failed at rewind, before reaching the transport a second time.
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("transport calls = %d, want 1", got)
	}
}

// A context canceled during the backoff wait must surface as context.Canceled, not
// the wrapped retryable-status error, so errors.Is(err, context.Canceled) holds.
func TestDoJSONCanceledDuringBackoffReturnsContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // retryable: forces a backoff wait before the retry
	}))
	defer srv.Close()

	c := New(srv.Client())
	c.BaseDelay = time.Hour // long enough that cancellation must win
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req = req.WithContext(ctx)

	done := make(chan error, 1)
	go func() { _, err := c.DoJSON(req, 1<<10); done <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled (cancellation must not be masked by the status error)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DoJSON did not return after cancel")
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
