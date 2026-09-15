package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/vbcherepanov/a2abridge/v4/internal/metrics"
)

// sendPushBudget is how long an enqueue may take while the webhook hangs.
const sendPushBudget = 500 * time.Millisecond

var fastRetryPolicy = retryPolicy{
	maxAttempts: pushMaxAttempts,
	baseDelay:   time.Millisecond,
	maxDelay:    4 * time.Millisecond,
	perAttempt:  2 * time.Second,
}

func newTestPushSender(t *testing.T, allowPrivate bool) *PushSender {
	t.Helper()
	s := NewPushSender(nil, allowPrivate, discardLog())
	s.policy = fastRetryPolicy
	t.Cleanup(func() { closePushSender(t, s) })
	return s
}

func closePushSender(t *testing.T, s *PushSender) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Errorf("close push sender: %v", err)
	}
}

func statusEvent(state a2a.TaskState) a2a.Event {
	return &a2a.TaskStatusUpdateEvent{TaskID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: state}}
}

func artifactEvent(text string) a2a.Event {
	return &a2a.TaskArtifactUpdateEvent{
		TaskID: "t1", ContextID: "c1",
		Artifact: &a2a.Artifact{ID: "a1", Parts: a2a.ContentParts{a2a.NewTextPart(text)}},
	}
}

// blockingWebhook accepts requests but answers only after release.
type blockingWebhook struct {
	url     string
	hits    atomic.Int32
	started chan struct{}
	release func()
}

func newBlockingWebhook(t *testing.T) *blockingWebhook {
	t.Helper()
	h := &blockingWebhook{started: make(chan struct{}, 16)}
	gate := make(chan struct{})
	var once sync.Once
	h.release = func() { once.Do(func() { close(gate) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.hits.Add(1)
		h.started <- struct{}{}
		<-gate
		w.WriteHeader(http.StatusNoContent)
	}))
	// Cleanups run LIFO: release the handlers first so srv.Close can return.
	t.Cleanup(srv.Close)
	t.Cleanup(h.release)
	h.url = srv.URL
	return h
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// pushFailedTotal reads the push failure counter from the metrics endpoint.
func pushFailedTotal(t *testing.T) int64 {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, "a2abridge_push_failed_total "); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatalf("parse push_failed_total %q: %v", v, err)
			}
			return n
		}
	}
	t.Fatal("a2abridge_push_failed_total not exported")
	return 0
}

func TestPushSenderRetries5xxThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := newTestPushSender(t, true).deliver(context.Background(), &a2a.PushConfig{URL: srv.URL}, []byte(`{}`))
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestPushSenderGivesUpAfterMaxAttempts(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := newTestPushSender(t, true).deliver(context.Background(), &a2a.PushConfig{URL: srv.URL}, []byte(`{}`)); err == nil {
		t.Fatal("deliver succeeded against a permanently failing webhook")
	}
	if got := hits.Load(); got != pushMaxAttempts {
		t.Fatalf("attempts = %d, want %d", got, pushMaxAttempts)
	}
}

func TestPushSenderNoRetryOn4xx(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	s := newTestPushSender(t, true)
	if err := s.deliver(context.Background(), &a2a.PushConfig{URL: srv.URL}, []byte(`{}`)); err == nil {
		t.Fatal("deliver succeeded on 400")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 (4xx is permanent)", got)
	}
	// A failed delivery must never fail the task: a2a-go treats a sender
	// error as an execution failure.
	if err := s.SendPush(context.Background(), &a2a.PushConfig{URL: srv.URL}, statusEvent(a2a.TaskStateCompleted)); err != nil {
		t.Fatalf("SendPush = %v, want nil on delivery failure", err)
	}
	closePushSender(t, s)
	if got := hits.Load(); got != 2 {
		t.Fatalf("attempts after SendPush = %d, want 2", got)
	}
}

func TestPushSenderSSRFGuard(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := &a2a.PushConfig{URL: srv.URL}

	err := newTestPushSender(t, false).deliver(context.Background(), cfg, []byte(`{}`))
	if !errors.Is(err, errBlockedPushTarget) {
		t.Fatalf("guarded deliver to %s = %v, want errBlockedPushTarget", srv.URL, err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("guarded sender reached the loopback webhook %d times", got)
	}

	if err := newTestPushSender(t, true).deliver(context.Background(), cfg, []byte(`{}`)); err != nil {
		t.Fatalf("deliver with private networks allowed: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
}

func TestPushSenderHeadersAndBody(t *testing.T) {
	type seen struct {
		token, auth, contentType string
		event                    a2a.Event
	}
	got := make(chan seen, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var sr a2a.StreamResponse
		if err := json.Unmarshal(body, &sr); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got <- seen{
			token:       r.Header.Get(pushTokenHeader),
			auth:        r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
			event:       sr.Event,
		}
	}))
	defer srv.Close()

	s := newTestPushSender(t, true)
	cases := []struct {
		name     string
		configID string
		auth     *a2a.PushAuthInfo
		wantAuth string
	}{
		{name: "bearer", configID: "cfg-bearer", auth: &a2a.PushAuthInfo{Scheme: "Bearer", Credentials: "secret-1"}, wantAuth: "Bearer secret-1"},
		{name: "basic", configID: "cfg-basic", auth: &a2a.PushAuthInfo{Scheme: "basic", Credentials: "dXNlcjpwYXNz"}, wantAuth: "Basic dXNlcjpwYXNz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &a2a.PushConfig{TaskID: "t1", ID: tc.configID, URL: srv.URL, Token: "tok-9", Auth: tc.auth}
			if err := s.SendPush(context.Background(), cfg, statusEvent(a2a.TaskStateCompleted)); err != nil {
				t.Fatal(err)
			}
			var h seen
			select {
			case h = <-got:
			case <-time.After(waitTimeout):
				t.Fatal("webhook not called")
			}
			if h.token != "tok-9" || h.auth != tc.wantAuth || h.contentType != "application/json" {
				t.Errorf("headers token=%q auth=%q type=%q", h.token, h.auth, h.contentType)
			}
			ev, ok := h.event.(*a2a.TaskStatusUpdateEvent)
			if !ok || ev.Status.State != a2a.TaskStateCompleted {
				t.Errorf("body event = %#v, want COMPLETED status update", h.event)
			}
		})
	}
}

func TestPushSenderHonorsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	s := newTestPushSender(t, true)
	s.policy = retryPolicy{maxAttempts: pushMaxAttempts, baseDelay: time.Hour, maxDelay: time.Hour, perAttempt: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := s.deliver(ctx, &a2a.PushConfig{URL: srv.URL}, []byte(`{}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deliver = %v, want context deadline", err)
	}
	if elapsed := time.Since(start); elapsed > waitTimeout {
		t.Fatalf("deliver ignored ctx for %s", elapsed)
	}
}

// TestPushSenderSendPushDoesNotWaitForWebhook: a hanging webhook must not
// stall the a2a-go event loop that calls SendPush.
func TestPushSenderSendPushDoesNotWaitForWebhook(t *testing.T) {
	hook := newBlockingWebhook(t)
	s := newTestPushSender(t, true)
	cfg := &a2a.PushConfig{TaskID: "t1", ID: "cfg", URL: hook.url}

	start := time.Now()
	for range 3 {
		if err := s.SendPush(context.Background(), cfg, statusEvent(a2a.TaskStateWorking)); err != nil {
			t.Fatalf("SendPush: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > sendPushBudget {
		t.Fatalf("SendPush blocked for %s while the webhook hangs", elapsed)
	}
	waitSignal(t, hook.started, "first delivery in flight")

	hook.release()
	closePushSender(t, s)
	if got := hook.hits.Load(); got != 3 {
		t.Fatalf("deliveries = %d, want 3", got)
	}
}

// TestPushSenderPreservesOrderPerConfig: one config's events arrive in the
// order a2a-go produced them, even when the first delivery is slow.
func TestPushSenderPreservesOrderPerConfig(t *testing.T) {
	var (
		mu       sync.Mutex
		received []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var sr a2a.StreamResponse
		if err := json.Unmarshal(body, &sr); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		label := ""
		switch ev := sr.Event.(type) {
		case *a2a.TaskArtifactUpdateEvent:
			label = ev.Artifact.Parts[0].Text()
		case *a2a.TaskStatusUpdateEvent:
			label = string(ev.Status.State)
		default:
		}
		mu.Lock()
		first := len(received) == 0
		received = append(received, label)
		mu.Unlock()
		if first {
			time.Sleep(30 * time.Millisecond)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := newTestPushSender(t, true)
	cfg := &a2a.PushConfig{TaskID: "t1", ID: "cfg", URL: srv.URL}
	want := make([]string, 0, 11)
	for i := range 10 {
		text := strconv.Itoa(i)
		want = append(want, text)
		if err := s.SendPush(context.Background(), cfg, artifactEvent(text)); err != nil {
			t.Fatal(err)
		}
	}
	want = append(want, string(a2a.TaskStateCompleted))
	if err := s.SendPush(context.Background(), cfg, statusEvent(a2a.TaskStateCompleted)); err != nil {
		t.Fatal(err)
	}
	closePushSender(t, s)

	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(received) != fmt.Sprint(want) {
		t.Fatalf("delivery order = %v, want %v", received, want)
	}
}

// TestPushSenderOverflowDropsAndCounts: past the queue bound events are
// dropped and counted as failed pushes instead of blocking the caller.
func TestPushSenderOverflowDropsAndCounts(t *testing.T) {
	hook := newBlockingWebhook(t)
	s := newTestPushSender(t, true)
	s.queueSize = 2
	cfg := &a2a.PushConfig{TaskID: "t1", ID: "cfg", URL: hook.url}
	send := func() {
		if err := s.SendPush(context.Background(), cfg, statusEvent(a2a.TaskStateWorking)); err != nil {
			t.Fatalf("SendPush: %v", err)
		}
	}

	send()
	waitSignal(t, hook.started, "first delivery in flight")
	failedBefore := pushFailedTotal(t)
	const dropped = 5
	for range s.queueSize + dropped {
		send()
	}
	if got := pushFailedTotal(t) - failedBefore; got != dropped {
		t.Fatalf("push_failed_total grew by %d, want %d", got, dropped)
	}

	hook.release()
	closePushSender(t, s)
	if got := hook.hits.Load(); got != int32(1+s.queueSize) {
		t.Fatalf("deliveries = %d, want %d (in-flight + queued)", got, 1+s.queueSize)
	}
}

// TestPushSenderCloseDrains: Close delivers everything already queued and
// refuses events afterwards.
func TestPushSenderCloseDrains(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := newTestPushSender(t, true)
	for i := range 5 {
		cfg := &a2a.PushConfig{TaskID: "t1", ID: "cfg-" + strconv.Itoa(i%2), URL: srv.URL}
		if err := s.SendPush(context.Background(), cfg, statusEvent(a2a.TaskStateWorking)); err != nil {
			t.Fatal(err)
		}
	}
	closePushSender(t, s)
	if got := hits.Load(); got != 5 {
		t.Fatalf("deliveries after Close = %d, want 5", got)
	}
	if n := s.activeWorkers(); n != 0 {
		t.Fatalf("workers after Close = %d, want 0", n)
	}

	failedBefore := pushFailedTotal(t)
	if err := s.SendPush(context.Background(), &a2a.PushConfig{TaskID: "t1", ID: "late", URL: srv.URL}, statusEvent(a2a.TaskStateCompleted)); err != nil {
		t.Fatalf("SendPush after Close = %v, want nil", err)
	}
	if got := pushFailedTotal(t) - failedBefore; got != 1 {
		t.Fatalf("push after Close counted %d failures, want 1", got)
	}
	if n := s.activeWorkers(); n != 0 {
		t.Fatalf("SendPush after Close started a worker")
	}
}

// TestPushSenderCloseDeadlineAbortsDelivery: a hanging webhook cannot hold
// shutdown past the drain deadline.
func TestPushSenderCloseDeadlineAbortsDelivery(t *testing.T) {
	hook := newBlockingWebhook(t)
	s := newTestPushSender(t, true)
	if err := s.SendPush(context.Background(), &a2a.PushConfig{TaskID: "t1", ID: "cfg", URL: hook.url}, statusEvent(a2a.TaskStateWorking)); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, hook.started, "delivery in flight")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := s.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > waitTimeout {
		t.Fatalf("Close took %s past its deadline", elapsed)
	}
}

// TestPushWorkerRetires: a worker exits after a terminal event and after
// the idle timeout.
func TestPushWorkerRetires(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := newTestPushSender(t, true)
	if err := s.SendPush(context.Background(), &a2a.PushConfig{TaskID: "t1", ID: "done", URL: srv.URL}, statusEvent(a2a.TaskStateCompleted)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "worker exit after terminal event", func() bool { return s.activeWorkers() == 0 && hits.Load() == 1 })

	s.mu.Lock()
	s.idleTimeout = 20 * time.Millisecond
	s.mu.Unlock()
	if err := s.SendPush(context.Background(), &a2a.PushConfig{TaskID: "t2", ID: "idle", URL: srv.URL}, statusEvent(a2a.TaskStateWorking)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "idle worker exit", func() bool { return s.activeWorkers() == 0 && hits.Load() == 2 })
}

func TestLimitPushRedirectsDropsTokenAcrossHosts(t *testing.T) {
	prev, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://a.example/hook", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	next, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://b.example/hook", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	next.Header.Set(pushTokenHeader, "tok")
	if err := limitPushRedirects(next, []*http.Request{prev}); err != nil {
		t.Fatal(err)
	}
	if next.Header.Get(pushTokenHeader) != "" {
		t.Error("token forwarded to a different host")
	}
	via := make([]*http.Request, pushMaxRedirects)
	for i := range via {
		via[i] = prev
	}
	if err := limitPushRedirects(next, via); err == nil {
		t.Error("redirect chain not bounded")
	}
}
