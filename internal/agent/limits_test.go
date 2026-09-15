package agent

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// bufferSlack allows for bytes that sit in socket and transport buffers
// between the client writer and the server's MaxBytesReader.
const bufferSlack = 32 << 20

// zeroReader streams up to limit zero bytes and counts how many were read.
type zeroReader struct {
	limit int64
	read  atomic.Int64
}

func (z *zeroReader) Read(p []byte) (int, error) {
	left := z.limit - z.read.Load()
	if left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > left {
		p = p[:left]
	}
	clear(p)
	z.read.Add(int64(len(p)))
	return len(p), nil
}

// TestOversizedBodyRejected: bodies past maxRequestBodyBytes get 413 on both
// bindings, JSON-RPC carrying an invalid-request error with a null id.
func TestOversizedBodyRejected(t *testing.T) {
	a := newTestAgent(t, "peer")
	oversized := bytes.Repeat([]byte("a"), maxRequestBodyBytes+1)

	rpc := rawCall(t, http.MethodPost, a.url+"/", "1.0", oversized)
	if rpc.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("JSON-RPC status = %d, want 413; body %.200s", rpc.status, rpc.body)
	}
	assertRPCError(t, rpc, "null", -32600, "INVALID_REQUEST")

	rest := rawCall(t, http.MethodPost, a.url+"/message:send", "1.0", oversized)
	assertRESTError(t, rest, http.StatusRequestEntityTooLarge, "INVALID_REQUEST")

	if n := len(a.store.PeekInbox()); n != 0 {
		t.Fatalf("oversized requests reached the inbox: %d", n)
	}
}

// TestOversizedStreamNotBuffered: an endless chunked body is cut off near
// the limit instead of being read into memory.
func TestOversizedStreamNotBuffered(t *testing.T) {
	a := newTestAgent(t, "peer")
	const offered = 256 << 20

	for _, path := range []string{"/", "/message:send"} {
		body := &zeroReader{limit: offered}
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url+path, body)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		req.ContentLength = -1 // chunked: the server cannot trust a length header
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(a2a.SvcParamVersion, "1.0")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Errorf("%s status = %d, want 413", path, resp.StatusCode)
			}
			if _, derr := io.Copy(io.Discard, resp.Body); derr != nil {
				t.Logf("%s drain: %v", path, derr)
			}
			resp.Body.Close()
		}
		cancel()
		if got := body.read.Load(); got >= maxRequestBodyBytes+bufferSlack {
			t.Fatalf("%s: client wrote %d bytes before the server stopped reading, want < %d", path, got, maxRequestBodyBytes+bufferSlack)
		}
	}
}

// TestLargeMessageUnderLimitAccepted: a message close to the limit still goes through.
func TestLargeMessageUnderLimitAccepted(t *testing.T) {
	a := newTestAgent(t, "peer")
	c := a.client(t)
	text := strings.Repeat("x", 1<<20)

	task := sendImmediate(t, c, userMessage(text))
	entry := a.inboxEntryFor(t, task.ID)
	if len(entry.Text) != len(text) {
		t.Fatalf("inbox text length = %d, want %d", len(entry.Text), len(text))
	}
	if err := a.executor.CompleteTask(string(task.ID), "ok"); err != nil {
		t.Fatal(err)
	}
}

// TestUnsupportedVersionRejectedForAnyBodyShape: a JSON-RPC body that is not
// a single valid request cannot slip past the version check.
func TestUnsupportedVersionRejectedForAnyBodyShape(t *testing.T) {
	a := newTestAgent(t, "peer")
	bodies := map[string]string{
		"batch":     `[{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x"}}]`,
		"object id": `{"jsonrpc":"2.0","id":{"a":1},"method":"GetTask","params":{"id":"x"}}`,
		"no id":     `{"jsonrpc":"2.0","method":"GetTask","params":{"id":"x"}}`,
		"garbage":   `not json at all`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			resp := rawCall(t, http.MethodPost, a.url+"/", "0.3", []byte(body))
			if resp.status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", resp.status, resp.body)
			}
			assertRPCError(t, resp, "null", -32009, "VERSION_NOT_SUPPORTED")
		})
	}
	// A usable id is still echoed.
	valid := rawCall(t, http.MethodPost, a.url+"/", "0.3", jsonrpcRequest(t, 11, "GetTask", map[string]any{"id": "x"}))
	assertRPCError(t, valid, "11", -32009, "VERSION_NOT_SUPPORTED")
}

// TestStreamsOutliveReadTimeout: http.Server.ReadTimeout bounds reading the
// request only; streams on both bindings stay open past it.
func TestStreamsOutliveReadTimeout(t *testing.T) {
	const readTimeout = 200 * time.Millisecond
	a := newTestAgentWithServer(t, "peer", func(s *http.Server) { s.ReadTimeout = readTimeout })
	c := a.client(t)

	// JSON-RPC streaming send (request with a body).
	errc := make(chan error, 1)
	go func() {
		entry := a.firstInboxEntry(t)
		time.Sleep(3 * readTimeout)
		errc <- a.executor.CompleteTask(entry.TaskID, "late reply")
	}()
	completed := false
	for ev, err := range c.SendStreamingMessage(context.Background(), &a2a.SendMessageRequest{Message: userMessage("slow")}) {
		if err != nil {
			t.Fatalf("streaming send broke after ReadTimeout: %v", err)
		}
		if isTerminalEvent(ev) {
			completed = true
		}
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !completed {
		t.Fatal("streaming send ended without the completion")
	}

	// REST subscribe (request without a body).
	task := sendImmediate(t, c, userMessage("watch"))
	waitTaskState(t, c, task.ID, a2a.TaskStateWorking)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, a.url+"/tasks/"+string(task.ID)+":subscribe", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(a2a.SvcParamVersion, "1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("REST subscribe: %v", err)
	}
	defer resp.Body.Close()
	go func() {
		time.Sleep(3 * readTimeout)
		errc <- a.executor.CompleteTask(string(task.ID), "late")
	}()
	sawCompleted := false
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), string(a2a.TaskStateCompleted)) {
			sawCompleted = true
			break
		}
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !sawCompleted {
		t.Fatalf("REST subscribe stream ended without the completion (scan err %v)", scanner.Err())
	}
}
