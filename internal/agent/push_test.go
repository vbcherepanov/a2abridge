package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vbcherepanov/a2abridge/internal/a2a"
)

func TestPushStoreCRUD(t *testing.T) {
	p := NewPushStore()
	ctx := context.Background()

	out, err := p.CreatePushConfig(ctx, a2a.TaskPushNotificationConfig{
		TaskID: "t1",
		Config: a2a.PushNotificationConfig{URL: "http://wh1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Config.ID == "" {
		t.Error("CreatePushConfig should assign an ID")
	}

	if _, err := p.CreatePushConfig(ctx, a2a.TaskPushNotificationConfig{
		TaskID: "t1",
		Config: a2a.PushNotificationConfig{URL: "http://wh2"},
	}); err != nil {
		t.Fatal(err)
	}

	list, err := p.ListPushConfigs(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("List size = %d, want 2", len(list))
	}

	if _, err := p.GetPushConfig(ctx, a2a.PushNotificationConfigParams{
		TaskID: "t1", PushConfigID: out.Config.ID,
	}); err != nil {
		t.Errorf("Get failed: %v", err)
	}

	// Get without id when multiple registered must error.
	if _, err := p.GetPushConfig(ctx, a2a.PushNotificationConfigParams{TaskID: "t1"}); err == nil {
		t.Error("Get without id with multiple configs should error")
	}

	if err := p.DeletePushConfig(ctx, a2a.PushNotificationConfigParams{
		TaskID: "t1", PushConfigID: out.Config.ID,
	}); err != nil {
		t.Errorf("Delete failed: %v", err)
	}

	list, _ = p.ListPushConfigs(ctx, "t1")
	if len(list) != 1 {
		t.Errorf("after Delete list size = %d, want 1", len(list))
	}

	// Delete-all must wipe the task entry.
	if err := p.DeletePushConfig(ctx, a2a.PushNotificationConfigParams{TaskID: "t1"}); err != nil {
		t.Errorf("Delete-all failed: %v", err)
	}
	list, _ = p.ListPushConfigs(ctx, "t1")
	if len(list) != 0 {
		t.Errorf("after Delete-all list size = %d, want 0", len(list))
	}
}

func TestPushStoreCreateValidation(t *testing.T) {
	p := NewPushStore()
	if _, err := p.CreatePushConfig(context.Background(), a2a.TaskPushNotificationConfig{
		Config: a2a.PushNotificationConfig{URL: "http://wh"},
	}); err == nil {
		t.Error("missing taskId should error")
	}
	if _, err := p.CreatePushConfig(context.Background(), a2a.TaskPushNotificationConfig{
		TaskID: "t1",
	}); err == nil {
		t.Error("missing url should error")
	}
}

func TestWebhookRetriesOn5xxThenSucceeds(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// First 2 attempts: 503; third: success. With our retry policy that
		// exercises both the backoff loop AND the eventual 2xx path.
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	cfg := a2a.PushNotificationConfig{URL: srv.URL}
	deliverWebhookWithPolicy(cfg, []byte(`{"x":1}`), retryPolicy{
		maxAttempts: 5,
		baseDelay:   1 * time.Millisecond,
		maxDelay:    8 * time.Millisecond,
		perAttempt:  500 * time.Millisecond,
	})

	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3 (2 retries + success)", got)
	}
}

func TestWebhookDoesNotRetryOn4xx(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	deliverWebhookWithPolicy(a2a.PushNotificationConfig{URL: srv.URL}, []byte(`{}`), retryPolicy{
		maxAttempts: 5,
		baseDelay:   1 * time.Millisecond,
		maxDelay:    8 * time.Millisecond,
		perAttempt:  500 * time.Millisecond,
	})

	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (4xx must not retry)", got)
	}
}

func TestPushStoreNotifyDelivers(t *testing.T) {
	var hits int32
	var lastBody atomic.Value
	var lastToken atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		body, _ := io.ReadAll(r.Body)
		lastBody.Store(body)
		lastToken.Store(r.Header.Get("X-A2A-Token"))
		w.WriteHeader(204)
	}))
	defer srv.Close()

	p := NewPushStore()
	if _, err := p.CreatePushConfig(context.Background(), a2a.TaskPushNotificationConfig{
		TaskID: "t1",
		Config: a2a.PushNotificationConfig{URL: srv.URL, Token: "secret-tok"},
	}); err != nil {
		t.Fatal(err)
	}

	p.Notify("t1", a2a.StreamResponse{
		StatusUpdate: &a2a.TaskStatusUpdateEvent{TaskID: "t1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}, Final: true},
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&hits) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("webhook hit %d times, want 1", hits)
	}
	if got := lastToken.Load(); got != "secret-tok" {
		t.Errorf("X-A2A-Token = %v, want secret-tok", got)
	}
	body, _ := lastBody.Load().([]byte)
	var ev a2a.StreamResponse
	if err := json.Unmarshal(body, &ev); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if ev.StatusUpdate == nil || ev.StatusUpdate.Status.State != a2a.TaskStateCompleted {
		t.Errorf("delivered event missing COMPLETED state: %+v", ev)
	}
}
