package cdp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"time"
)

// defaultLaunchTimeout bounds the Browser.getVersion handshake. On timeout,
// Spawn kills the process group and closes the pipes.
const defaultLaunchTimeout = 60 * time.Second

// waitDelay bounds how long cmd.Wait blocks on the stderr-copy goroutine after the
// process has exited, so a lingering child holding fd 2 cannot block the reaper.
const waitDelay = 5 * time.Second

// SpawnOptions configures Spawn.
type SpawnOptions struct {
	LaunchTimeout time.Duration // version-handshake budget; 0 uses defaultLaunchTimeout
	StderrMax     int           // crash-diagnostics ring size; 0 uses defaultStderrMax
	Logger        *slog.Logger  // nil discards
}

// baseArgs are the Chromium flags WaxSeal launches with. They match the previous
// launcher's defaults plus WaxSeal's explicit flags, with remote-debugging-port
// replaced by remote-debugging-pipe and automation/watchdog flags omitted. The
// captured source argv lives in testdata/argv_baseline.txt. Several flags affect
// fingerprinting and process cleanup, so TestArgvGolden pins the set.
var baseArgs = []string{
	"--remote-debugging-pipe",
	"--no-sandbox",
	"--disable-dev-shm-usage",
	"--disable-gpu",
	"--mute-audio",
	"--disable-blink-features=AutomationControlled",
	"--no-first-run",
	"--no-startup-window",
	"--disable-features=site-per-process,TranslateUI",
	"--disable-background-networking",
	"--disable-background-timer-throttling",
	"--disable-backgrounding-occluded-windows",
	"--disable-breakpad",
	"--disable-client-side-phishing-detection",
	"--disable-component-extensions-with-background-pages",
	"--disable-default-apps",
	"--disable-hang-monitor",
	"--disable-ipc-flooding-protection",
	"--disable-popup-blocking",
	"--disable-prompt-on-repost",
	"--disable-renderer-backgrounding",
	"--disable-sync",
	"--disable-site-isolation-trials",
	"--enable-features=NetworkService,NetworkServiceInProcess",
	"--force-color-profile=srgb",
	"--metrics-recording-only",
	"--use-mock-keychain",
}

// BuildArgs assembles the sorted Chromium argv for a profile directory. headful
// drops the headless flag; otherwise headless=new is used. The result is sorted
// so it is order-independent.
func BuildArgs(profileDir string, headful bool) []string {
	args := make([]string, 0, len(baseArgs)+2)
	args = append(args, baseArgs...)
	args = append(args, "--user-data-dir="+profileDir)
	if !headful {
		args = append(args, "--headless=new")
	}
	sort.Strings(args)
	return args
}

// Spawn launches bin with args over a remote-debugging pipe and completes the
// Browser.getVersion handshake. On any failure it kills the process group and
// closes the pipes, so failed starts do not leave the launched process running.
// The caller owns Browser.Close.
func Spawn(ctx context.Context, bin string, args []string, opts SpawnOptions) (*Browser, error) {
	if !platformSupported {
		return nil, errors.New("cdp: pipe transport unsupported on Windows")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.LaunchTimeout <= 0 {
		opts.LaunchTimeout = defaultLaunchTimeout
	}
	stderrMax := opts.StderrMax
	if stderrMax <= 0 {
		stderrMax = defaultStderrMax
	}

	// Pipe A: parent writes commands -> child reads fd 3.
	// Pipe B: child writes fd 4 -> parent reads responses/events.
	aR, aW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("cdp: command pipe: %w", err)
	}
	bR, bW, err := os.Pipe()
	if err != nil {
		_ = aR.Close()
		_ = aW.Close()
		return nil, fmt.Errorf("cdp: event pipe: %w", err)
	}

	cmd := exec.Command(bin, args...)
	cmd.ExtraFiles = []*os.File{aR, bW} // child fd 3 = aR, fd 4 = bW
	stderr := &ringBuffer{max: stderrMax}
	cmd.Stderr = stderr
	// Stderr uses a ringBuffer, so os/exec copies from a pipe in a goroutine that
	// Wait joins. If a Chromium helper inherits fd 2 and outlives the main process,
	// that pipe can stay open after the parent exits. WaitDelay bounds the join.
	cmd.WaitDelay = waitDelay
	setSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		_ = aR.Close()
		_ = aW.Close()
		_ = bR.Close()
		_ = bW.Close()
		return nil, fmt.Errorf("cdp: start chromium: %w", err)
	}
	// Close the parent's copies of the child-side ends. A lingering child-side
	// write end would keep the event pipe from ever reaching EOF on Chromium exit.
	_ = aR.Close()
	_ = bW.Close()

	c := &Conn{
		cmd:     cmd,
		wpipe:   aW,
		rpipe:   bR,
		log:     opts.Logger,
		closeCh: make(chan struct{}),
		pending: make(map[int64]chan rpcResult),
	}
	go c.readLoop()
	go func() {
		// Reap the process and signal exit even if the read loop has not yet seen
		// EOF (e.g. the process was group-killed). Record the reap before tearing
		// down so killProcessGroup never signals a PID the OS may have recycled.
		_ = cmd.Wait()
		c.procExited.Store(true)
		c.teardown(fmt.Errorf("%w: process exited", ErrConnClosed))
	}()

	b := &Browser{conn: c, ctx: context.Background()}
	hctx, cancel := context.WithTimeout(ctx, opts.LaunchTimeout)
	defer cancel()
	if _, err := b.Context(hctx).Version(); err != nil {
		c.forceClose(fmt.Errorf("version handshake: %w", err))
		return nil, fmt.Errorf("cdp: launch handshake: %w (stderr tail: %q)", err, tail(stderr.String(), 600))
	}
	return b, nil
}

// tail returns the last n characters of s.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
