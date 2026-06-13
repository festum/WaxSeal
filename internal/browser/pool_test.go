package browser

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests replace newInstance and use partially initialized browserInstance
// values to exercise pool recovery without launching Chromium.

// Concurrent callers that observed the same stale instance share one relaunch.
func TestPoolRelaunchSingleFlight(t *testing.T) {
	stale := &browserInstance{}
	var created int64
	var once sync.Once
	started := make(chan struct{})
	release := make(chan struct{})
	p := &Pool{opts: withDefaults(Options{}), cur: stale}
	p.newInstance = func() (*browserInstance, error) {
		atomic.AddInt64(&created, 1)
		once.Do(func() { close(started) })
		<-release // Keep the relaunch in progress until all callers are waiting.
		return &browserInstance{}, nil
	}

	const n = 8
	var wg sync.WaitGroup
	results := make([]*browserInstance, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = p.relaunch(stale) }(i)
	}
	<-started
	time.Sleep(25 * time.Millisecond) // Allow the remaining callers to begin waiting.
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(&created); got != 1 {
		t.Errorf("newInstance called %d times, want 1", got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("relaunch[%d] err = %v", i, errs[i])
		}
		if results[i] != p.cur {
			t.Errorf("relaunch[%d] did not return the current instance", i)
		}
	}
}

// A caller with an obsolete snapshot receives the current instance.
func TestPoolRelaunchShortCircuitsWhenAlreadySwapped(t *testing.T) {
	stale := &browserInstance{}
	fresh := &browserInstance{}
	var created int64
	p := &Pool{opts: withDefaults(Options{}), cur: fresh}
	p.newInstance = func() (*browserInstance, error) { atomic.AddInt64(&created, 1); return &browserInstance{}, nil }

	got, err := p.relaunch(stale)
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if got != fresh {
		t.Error("relaunch should return the already-current instance")
	}
	if atomic.LoadInt64(&created) != 0 {
		t.Error("newInstance was called after the stale instance was replaced")
	}
}

// A failed relaunch starts a backoff window before another launch is allowed.
func TestPoolRelaunchBackoffAfterFailure(t *testing.T) {
	stale := &browserInstance{}
	var created int64
	p := &Pool{opts: withDefaults(Options{}), cur: stale}
	p.newInstance = func() (*browserInstance, error) {
		atomic.AddInt64(&created, 1)
		return nil, errors.New("launch failed")
	}

	if _, err := p.relaunch(stale); err == nil {
		t.Fatal("first relaunch should return the launch error")
	}
	if got := atomic.LoadInt64(&created); got != 1 {
		t.Fatalf("newInstance calls = %d, want 1", got)
	}
	if _, err := p.relaunch(stale); err == nil {
		t.Fatal("second relaunch within the backoff window should fail fast")
	}
	if got := atomic.LoadInt64(&created); got != 1 {
		t.Errorf("newInstance calls during backoff = %d, want 1", got)
	}
}

// The relaunch delay grows exponentially up to relaunchBackoffMax.
func TestPoolBackoffWindow(t *testing.T) {
	p := &Pool{}
	for _, c := range []struct {
		fails int
		want  time.Duration
	}{
		{1, relaunchBackoffBase},
		{2, 2 * relaunchBackoffBase},
		{3, 4 * relaunchBackoffBase},
		{100, relaunchBackoffMax},
	} {
		p.relaunchStreak = c.fails
		if got := p.backoffWindow(); got != c.want {
			t.Errorf("backoffWindow(streak=%d) = %v, want %v", c.fails, got, c.want)
		}
	}
}

// A browser that survives relaunchStableWindow starts a new backoff streak.
func TestPoolRelaunchResetsStreakAfterStableGap(t *testing.T) {
	stale := &browserInstance{}
	var created int64
	p := &Pool{opts: withDefaults(Options{}), cur: stale, relaunchStreak: 5, lastRelaunchAt: time.Now().Add(-time.Hour)}
	p.newInstance = func() (*browserInstance, error) { atomic.AddInt64(&created, 1); return &browserInstance{}, nil }
	if _, err := p.relaunch(stale); err != nil {
		t.Fatalf("a relaunch after a long stable gap must not back off: %v", err)
	}
	if got := atomic.LoadInt64(&created); got != 1 {
		t.Errorf("newInstance calls = %d, want 1", got)
	}
	if p.relaunchStreak != 1 {
		t.Errorf("relaunchStreak after stability window = %d, want 1", p.relaunchStreak)
	}
}

// Waiting through the maximum backoff must not reset an active crash-loop streak.
func TestPoolRelaunchHoldsCeilingDuringCrashLoop(t *testing.T) {
	stale := &browserInstance{}
	// The previous relaunch is old enough to clear the maximum backoff, but not the
	// stability window.
	p := &Pool{opts: withDefaults(Options{}), cur: stale, relaunchStreak: 5, lastRelaunchAt: time.Now().Add(-(relaunchBackoffMax + time.Second))}
	p.newInstance = func() (*browserInstance, error) { return &browserInstance{}, nil }
	if _, err := p.relaunch(stale); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if p.relaunchStreak <= 1 {
		t.Errorf("relaunchStreak after maximum backoff = %d, want greater than 1", p.relaunchStreak)
	}
}

// A browser that starts and dies during setup still contributes to the backoff.
func TestPoolRelaunchBackoffOnCrashLoop(t *testing.T) {
	stale := &browserInstance{}
	var created int64
	p := &Pool{opts: withDefaults(Options{}), cur: stale}
	p.newInstance = func() (*browserInstance, error) {
		atomic.AddInt64(&created, 1)
		return &browserInstance{}, nil
	}
	inst1, err := p.relaunch(stale)
	if err != nil {
		t.Fatalf("first relaunch: %v", err)
	}
	if _, err := p.relaunch(inst1); err == nil {
		t.Fatal("immediate relaunch after a post-launch crash should be rejected")
	}
	if got := atomic.LoadInt64(&created); got != 1 {
		t.Errorf("newInstance calls = %d, want 1", got)
	}
}

// Relaunch must not restore a closed pool.
func TestPoolRelaunchClosedBlocks(t *testing.T) {
	stale := &browserInstance{}
	p := &Pool{opts: withDefaults(Options{}), cur: stale}
	p.newInstance = func() (*browserInstance, error) { return &browserInstance{}, nil }
	p.Close()
	if _, err := p.relaunch(stale); !errors.Is(err, errPoolClosed) {
		t.Errorf("relaunch after Close = %v, want errPoolClosed", err)
	}
}

// Concurrent relaunch callers tear down the stale instance exactly once.
func TestPoolRelaunchDisposesStaleOnce(t *testing.T) {
	var teardowns int64
	stale := &browserInstance{onTeardown: func() { atomic.AddInt64(&teardowns, 1) }}
	p := &Pool{opts: withDefaults(Options{}), cur: stale}
	p.newInstance = func() (*browserInstance, error) { return &browserInstance{}, nil }

	const n = 6
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = p.relaunch(stale) }()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&teardowns); got != 1 {
		t.Errorf("stale teardown count = %d, want 1", got)
	}
}

// Close discards and tears down a replacement that finishes concurrently.
func TestPoolCloseDuringRelaunch(t *testing.T) {
	var newTorn int64
	stale := &browserInstance{}
	enter := make(chan struct{})
	release := make(chan struct{})
	p := &Pool{opts: withDefaults(Options{}), cur: stale}
	p.newInstance = func() (*browserInstance, error) {
		close(enter)
		<-release
		return &browserInstance{onTeardown: func() { atomic.AddInt64(&newTorn, 1) }}, nil
	}

	done := make(chan struct{})
	var relErr error
	go func() { _, relErr = p.relaunch(stale); close(done) }()

	<-enter
	p.Close()
	close(release)
	<-done

	if !errors.Is(relErr, errPoolClosed) {
		t.Errorf("relaunch during Close = %v, want errPoolClosed", relErr)
	}
	if got := atomic.LoadInt64(&newTorn); got != 1 {
		t.Errorf("replacement teardown count = %d, want 1", got)
	}
	if p.cur != nil {
		t.Error("cur should be nil after Close")
	}
}

// teardown runs at most once when Close races with relaunch.
func TestBrowserInstanceTeardownIdempotent(t *testing.T) {
	var n int64
	inst := &browserInstance{onTeardown: func() { atomic.AddInt64(&n, 1) }}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); inst.teardown() }()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&n); got != 1 {
		t.Errorf("teardown ran %d times, want 1", got)
	}
}
