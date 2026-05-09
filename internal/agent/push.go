package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vbcherepanov/a2abridge/internal/a2a"
)

// PushStore implements a2a.PushHandler. Per A2A 1.0 §9.5, peers register
// webhook URLs so they receive task state updates without keeping an SSE
// stream open. We persist configs in-memory only — bridges are short-lived
// (they die with the IDE session), so survival across restarts isn't worth
// the complexity. Configs are scoped per task id.
type PushStore struct {
	mu      sync.Mutex
	configs map[string]map[string]a2a.PushNotificationConfig // taskID -> configID -> config
}

// NewPushStore — fresh empty registry.
func NewPushStore() *PushStore {
	return &PushStore{configs: map[string]map[string]a2a.PushNotificationConfig{}}
}

// CreatePushConfig registers a new webhook for the named task.
func (p *PushStore) CreatePushConfig(_ context.Context, in a2a.TaskPushNotificationConfig) (*a2a.TaskPushNotificationConfig, error) {
	if in.TaskID == "" {
		return nil, errors.New("taskId required")
	}
	if in.Config.URL == "" {
		return nil, errors.New("pushNotificationConfig.url required")
	}
	cfg := in.Config
	if cfg.ID == "" {
		cfg.ID = uuid.NewString()
	}
	p.mu.Lock()
	if p.configs[in.TaskID] == nil {
		p.configs[in.TaskID] = map[string]a2a.PushNotificationConfig{}
	}
	p.configs[in.TaskID][cfg.ID] = cfg
	p.mu.Unlock()
	return &a2a.TaskPushNotificationConfig{TaskID: in.TaskID, Config: cfg}, nil
}

// GetPushConfig returns one config by id; empty PushConfigID means "the
// only one for this task" (errors if multiple are registered).
func (p *PushStore) GetPushConfig(_ context.Context, in a2a.PushNotificationConfigParams) (*a2a.TaskPushNotificationConfig, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	configs, ok := p.configs[in.TaskID]
	if !ok || len(configs) == 0 {
		return nil, a2a.ErrTaskNotFound
	}
	if in.PushConfigID == "" {
		if len(configs) != 1 {
			return nil, errors.New("pushNotificationConfigId required when multiple configs are registered")
		}
		for _, cfg := range configs {
			return &a2a.TaskPushNotificationConfig{TaskID: in.TaskID, Config: cfg}, nil
		}
	}
	cfg, ok := configs[in.PushConfigID]
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	return &a2a.TaskPushNotificationConfig{TaskID: in.TaskID, Config: cfg}, nil
}

// ListPushConfigs returns every config registered for the task. Empty
// taskID returns the flat list of every registered config across tasks
// (useful for diagnostics).
func (p *PushStore) ListPushConfigs(_ context.Context, taskID string) ([]a2a.TaskPushNotificationConfig, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []a2a.TaskPushNotificationConfig{}
	if taskID == "" {
		for tid, configs := range p.configs {
			for _, cfg := range configs {
				out = append(out, a2a.TaskPushNotificationConfig{TaskID: tid, Config: cfg})
			}
		}
		return out, nil
	}
	for _, cfg := range p.configs[taskID] {
		out = append(out, a2a.TaskPushNotificationConfig{TaskID: taskID, Config: cfg})
	}
	return out, nil
}

// DeletePushConfig removes a registered webhook. Empty PushConfigID
// removes ALL webhooks for the task — matches the spec's "delete all" intent.
func (p *PushStore) DeletePushConfig(_ context.Context, in a2a.PushNotificationConfigParams) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.configs[in.TaskID]; !ok {
		return a2a.ErrTaskNotFound
	}
	if in.PushConfigID == "" {
		delete(p.configs, in.TaskID)
		return nil
	}
	delete(p.configs[in.TaskID], in.PushConfigID)
	if len(p.configs[in.TaskID]) == 0 {
		delete(p.configs, in.TaskID)
	}
	return nil
}

// Notify is called by the bridge whenever a task state changes. It POSTs
// the event body to every webhook registered for that task. Failures are
// best-effort — a 500 from a peer's webhook does not affect the task.
//
// The body shape mirrors what we'd send over SSE: `{ "event": <event-type>,
// "data": <event-payload> }` so subscribers can write a single dispatcher
// regardless of transport.
func (p *PushStore) Notify(taskID string, ev a2a.StreamResponse) {
	p.mu.Lock()
	configs := p.configs[taskID]
	if len(configs) == 0 {
		p.mu.Unlock()
		return
	}
	// Snapshot to avoid holding the lock during HTTP I/O.
	snapshot := make([]a2a.PushNotificationConfig, 0, len(configs))
	for _, cfg := range configs {
		snapshot = append(snapshot, cfg)
	}
	p.mu.Unlock()

	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}

	for _, cfg := range snapshot {
		go deliverWebhook(cfg, payload)
	}
}

// retryPolicy defines the per-attempt behaviour for webhook delivery.
// Total worst-case time is sum(delays) + maxAttempts * perAttemptTimeout
// = 200+400+800+1600+3200 ms + 5*5s = 31.2s before giving up.
//
// We deliberately keep this short — webhooks are a fan-out side effect,
// not a guaranteed-delivery channel.
type retryPolicy struct {
	maxAttempts int
	baseDelay   time.Duration
	maxDelay    time.Duration
	perAttempt  time.Duration
}

var defaultRetryPolicy = retryPolicy{
	maxAttempts: 5,
	baseDelay:   200 * time.Millisecond,
	maxDelay:    3200 * time.Millisecond,
	perAttempt:  5 * time.Second,
}

// deliverWebhook posts the event body with exponential backoff retry.
// Retried statuses: any 5xx, plus network errors / timeouts. 4xx is
// treated as a permanent client mistake and not retried — re-sending the
// same payload that caused 400 won't make it 200.
func deliverWebhook(cfg a2a.PushNotificationConfig, body []byte) {
	deliverWebhookWithPolicy(cfg, body, defaultRetryPolicy)
}

// deliverWebhookWithPolicy is the explicit-policy variant used by tests
// to dial down delays without touching globals.
func deliverWebhookWithPolicy(cfg a2a.PushNotificationConfig, body []byte, policy retryPolicy) {
	delay := policy.baseDelay
	for attempt := 1; attempt <= policy.maxAttempts; attempt++ {
		ok, retryable := postOnce(cfg, body, policy.perAttempt)
		if ok {
			return
		}
		if !retryable {
			return // permanent failure (4xx, malformed URL, etc.)
		}
		if attempt == policy.maxAttempts {
			return
		}
		time.Sleep(delay)
		delay *= 2
		if delay > policy.maxDelay {
			delay = policy.maxDelay
		}
	}
}

// postOnce returns (delivered, retryable). delivered=true means the peer
// accepted the webhook (2xx); retryable=true means we should try again
// on a network/5xx failure.
func postOnce(cfg a2a.PushNotificationConfig, body []byte, perAttempt time.Duration) (bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), perAttempt)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return false, false // malformed URL: not retryable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", a2a.ProtocolVersion)
	if cfg.Token != "" {
		req.Header.Set("X-A2A-Token", cfg.Token)
	}
	if cfg.Authentication != nil && cfg.Authentication.Credentials != "" {
		// Spec doesn't pin the header name; "Authorization" is the
		// pragmatic default. Schemes like Basic / Bearer are typically
		// embedded in the credentials string itself.
		req.Header.Set("Authorization", fmt.Sprintf("%s %s",
			firstScheme(cfg.Authentication.Schemes), cfg.Authentication.Credentials))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Network error / timeout — retry.
		return false, true
	}
	resp.Body.Close()

	if resp.StatusCode/100 == 2 {
		return true, false
	}
	if resp.StatusCode >= 500 {
		return false, true
	}
	// 3xx redirects: stdlib already followed; if we landed here, it's
	// likely a 4xx. 4xx = permanent.
	return false, false
}

// firstScheme returns the first auth scheme name for the Authorization
// header, defaulting to "Bearer" when the schemes list is empty.
func firstScheme(schemes []string) string {
	if len(schemes) == 0 {
		return "Bearer"
	}
	return schemes[0]
}
