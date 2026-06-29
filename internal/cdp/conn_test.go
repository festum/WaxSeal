package cdp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// newPipeConn wires a Conn to two os.Pipes so a test can act as the browser side:
// it reads outgoing requests from reqR and writes responses to respW.
func newPipeConn(t *testing.T) (c *Conn, reqR *os.File, respW *os.File) {
	t.Helper()
	// Conn reads responses/events from rR (we write rW); Conn writes requests to wW
	// (we read wR).
	rR, rW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	wR, wW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c = &Conn{
		wpipe:   wW,
		rpipe:   rR,
		log:     slog.New(slog.DiscardHandler),
		closeCh: make(chan struct{}),
		pending: make(map[int64]chan rpcResult),
	}
	go c.readLoop()
	t.Cleanup(func() { _ = rR.Close(); _ = rW.Close(); _ = wR.Close(); _ = wW.Close() })
	return c, wR, rW
}

// readRequestID reads one NUL-delimited request frame and returns its id.
func readRequestID(t *testing.T, r *bufio.Reader) int64 {
	t.Helper()
	frame, err := r.ReadBytes(0)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	frame = frame[:len(frame)-1] // strip NUL
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(frame, &req); err != nil {
		t.Fatalf("parse request %q: %v", frame, err)
	}
	return req.ID
}

// Protocol error data can be any JSON value. Object data must still complete the
// pending call with rpcError instead of dropping the frame and leaving the call
// open.
func TestConnErrorWithObjectDataCompletesCall(t *testing.T) {
	c, reqR, respW := newPipeConn(t)
	br := bufio.NewReader(reqR)

	done := make(chan error, 1)
	go func() {
		_, err := c.rawCall(context.Background(), "", "Target.createBrowserContext", nil)
		done <- err
	}()

	id := readRequestID(t, br)
	resp := fmt.Sprintf(`{"id":%d,"error":{"code":-32000,"message":"boom","data":{"foo":"bar","n":1}}}`, id)
	if _, err := respW.Write(append([]byte(resp), 0)); err != nil {
		t.Fatalf("write response: %v", err)
	}

	select {
	case err := <-done:
		var re *rpcError
		if !errors.As(err, &re) {
			t.Fatalf("rawCall error = %v; want *rpcError", err)
		}
		if re.Code != -32000 || !strings.Contains(re.Message, "boom") {
			t.Errorf("rpcError = %+v; want code -32000 message containing boom", re)
		}
		if !strings.Contains(re.Error(), "foo") {
			t.Errorf("Error() = %q; want it to include the object data", re.Error())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rawCall hung: an error frame with object data was dropped instead of completing the call")
	}
}

// String error data (the common case) must still round-trip.
func TestConnErrorWithStringDataCompletesCall(t *testing.T) {
	c, reqR, respW := newPipeConn(t)
	br := bufio.NewReader(reqR)

	done := make(chan error, 1)
	go func() {
		_, err := c.rawCall(context.Background(), "", "Some.method", nil)
		done <- err
	}()

	id := readRequestID(t, br)
	resp := fmt.Sprintf(`{"id":%d,"error":{"code":-32000,"message":"nope","data":"detail text"}}`, id)
	if _, err := respW.Write(append([]byte(resp), 0)); err != nil {
		t.Fatalf("write response: %v", err)
	}

	select {
	case err := <-done:
		var re *rpcError
		if !errors.As(err, &re) {
			t.Fatalf("rawCall error = %v; want *rpcError", err)
		}
		if !strings.Contains(re.Error(), "detail text") {
			t.Errorf("Error() = %q; want it to include the string data", re.Error())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rawCall hung on a string-data error frame")
	}
}

// A successful response must complete the call with the result bytes.
func TestConnResultCompletesCall(t *testing.T) {
	c, reqR, respW := newPipeConn(t)
	br := bufio.NewReader(reqR)

	done := make(chan rpcResult, 1)
	go func() {
		raw, err := c.rawCall(context.Background(), "", "Browser.getVersion", nil)
		done <- rpcResult{result: raw, err: err}
	}()

	id := readRequestID(t, br)
	resp := fmt.Sprintf(`{"id":%d,"result":{"product":"Chrome/149"}}`, id)
	if _, err := respW.Write(append([]byte(resp), 0)); err != nil {
		t.Fatalf("write response: %v", err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("rawCall err = %v", r.err)
		}
		if !strings.Contains(string(r.result), "Chrome/149") {
			t.Errorf("result = %s; want it to carry the product", r.result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rawCall hung on a normal result frame")
	}
}
