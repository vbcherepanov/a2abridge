package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"

	"github.com/vbcherepanov/a2abridge/v4/internal/metrics"
)

// Push delivery knobs. Worst case per event is sum(delays) +
// pushMaxAttempts*pushAttemptTimeout = 200+400+800+1600 ms + 5*5 s = 28 s.
// Webhooks are a fan-out side effect, not a guaranteed-delivery channel.
const (
	pushMaxAttempts       = 5
	pushBaseDelay         = 200 * time.Millisecond
	pushMaxDelay          = 3200 * time.Millisecond
	pushAttemptTimeout    = 5 * time.Second
	pushMaxRedirects      = 10
	pushDialTimeout       = 30 * time.Second
	pushDialKeepAlive     = 30 * time.Second
	pushIdleConnTimeout   = 90 * time.Second
	pushTLSHandshake      = 10 * time.Second
	pushExpectContinue    = time.Second
	pushMaxIdleConns      = 100
	pushResponseDrainSize = 64 << 10
	pushTokenHeader       = "A2A-Notification-Token"

	// pushQueueSize bounds the events waiting for one push config; events
	// for a webhook that falls this far behind are dropped.
	pushQueueSize = 64

	// pushWorkerIdleTimeout retires a config's worker when no event arrives.
	pushWorkerIdleTimeout = 2 * time.Minute
)

// errBlockedPushTarget is returned when a webhook URL resolves to a
// non-public address range (SSRF protection, CWE-918).
var errBlockedPushTarget = errors.New("push notification target resolves to a blocked address range")

// retryPolicy is the per-sender delivery policy; tests shrink the delays.
type retryPolicy struct {
	maxAttempts int
	baseDelay   time.Duration
	maxDelay    time.Duration
	perAttempt  time.Duration
}

var defaultRetryPolicy = retryPolicy{
	maxAttempts: pushMaxAttempts,
	baseDelay:   pushBaseDelay,
	maxDelay:    pushMaxDelay,
	perAttempt:  pushAttemptTimeout,
}

// pushJob is one serialized event waiting for delivery.
type pushJob struct {
	config   a2a.PushConfig
	body     []byte
	terminal bool
}

// pushWorkerKey identifies one webhook registration.
type pushWorkerKey struct {
	taskID   a2a.TaskID
	configID string
}

type pushWorker struct {
	queue chan pushJob
}

// PushSender delivers task events to webhooks registered per A2A 1.0 push
// notifications. SendPush only enqueues, so a slow or dead webhook never
// stalls task event processing: every push config gets one worker that
// delivers its events in order, retrying network errors and 5xx with backoff,
// never 4xx. Delivery failures are logged and counted but never fail the task.
type PushSender struct {
	client *http.Client
	policy retryPolicy
	log    *slog.Logger

	queueSize   int
	idleTimeout time.Duration

	// ctx bounds every delivery; Close cancels it once draining ends.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closed  bool
	workers map[pushWorkerKey]*pushWorker
	wg      sync.WaitGroup
}

var _ push.Sender = (*PushSender)(nil)

// NewPushSender builds a sender whose HTTP client presents tlsConfig
// (federation mTLS; nil for plain loopback). Unless allowPrivateNetworks is
// set, every dial — including redirect hops — is refused when the resolved
// address is loopback, private, link-local, multicast or unspecified.
// Callers must Close the sender to drain pending deliveries.
func NewPushSender(tlsConfig *tls.Config, allowPrivateNetworks bool, log *slog.Logger) *PushSender {
	dialer := &net.Dialer{Timeout: pushDialTimeout, KeepAlive: pushDialKeepAlive}
	if !allowPrivateNetworks {
		dialer.Control = guardPushDial
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          pushMaxIdleConns,
		IdleConnTimeout:       pushIdleConnTimeout,
		TLSHandshakeTimeout:   pushTLSHandshake,
		ExpectContinueTimeout: pushExpectContinue,
	}
	if tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig.Clone()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PushSender{
		client:      &http.Client{Transport: transport, CheckRedirect: limitPushRedirects},
		policy:      defaultRetryPolicy,
		log:         log,
		queueSize:   pushQueueSize,
		idleTimeout: pushWorkerIdleTimeout,
		ctx:         ctx,
		cancel:      cancel,
		workers:     map[pushWorkerKey]*pushWorker{},
	}
}

// SendPush implements push.Sender. It queues the event for the config's
// worker and returns at once. a2a-go fails the task when a sender returns an
// error, so problems are logged and counted and nil is always returned.
func (s *PushSender) SendPush(_ context.Context, config *a2a.PushConfig, event a2a.Event) error {
	if config == nil {
		s.log.Warn("push skipped: nil config")
		return nil
	}
	body, err := json.Marshal(a2a.StreamResponse{Event: event})
	if err != nil {
		metrics.IncPushFailed()
		s.log.Warn("push event encode failed", "task", config.TaskID, "err", err)
		return nil
	}
	job := pushJob{config: copyPushConfig(config), body: body, terminal: isTerminalEvent(event)}
	key := pushWorkerKey{taskID: config.TaskID, configID: config.ID}

	// Enqueue under the lock: a worker retires only with the lock held and
	// an empty queue, so a queued job is never left without a worker.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		metrics.IncPushFailed()
		s.log.Warn("push dropped: sender closed", "task", config.TaskID, "config", config.ID)
		return nil
	}
	w, ok := s.workers[key]
	if !ok {
		w = &pushWorker{queue: make(chan pushJob, s.queueSize)}
		s.workers[key] = w
		s.wg.Add(1)
		go s.run(key, w)
	}
	select {
	case w.queue <- job:
	default:
		metrics.IncPushFailed()
		s.log.Warn("push dropped: delivery queue full", "task", config.TaskID, "config", config.ID, "queue", s.queueSize)
	}
	return nil
}

// Close stops accepting events and lets every worker deliver what is already
// queued. It returns once the workers are done or ctx is done; in the latter
// case in-flight and remaining deliveries are aborted before it returns.
// Safe to call multiple times.
func (s *PushSender) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		for _, w := range s.workers {
			close(w.queue)
		}
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		s.cancel()
		return nil
	case <-ctx.Done():
		s.cancel()
		<-done
		return fmt.Errorf("push sender drain: %w", ctx.Err())
	}
}

// run delivers one config's events in order until the config's task reached
// a terminal state, the worker stayed idle for idleTimeout, or Close.
func (s *PushSender) run(key pushWorkerKey, w *pushWorker) {
	defer s.wg.Done()
	idle := time.NewTimer(s.idleTimeout)
	defer idle.Stop()
	for {
		select {
		case job, ok := <-w.queue:
			if !ok {
				// Closed by Close and fully drained: unregister and exit.
				s.retire(key, w)
				return
			}
			s.deliverJob(&job)
			if job.terminal && s.retire(key, w) {
				return
			}
		case <-idle.C:
			if s.retire(key, w) {
				return
			}
		}
		idle.Reset(s.idleTimeout)
	}
}

// retire unregisters the worker when nothing is queued for it.
func (s *PushSender) retire(key pushWorkerKey, w *pushWorker) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(w.queue) > 0 {
		return false
	}
	if s.workers[key] == w {
		delete(s.workers, key)
	}
	return true
}

// activeWorkers reports how many push configs currently have a worker.
func (s *PushSender) activeWorkers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.workers)
}

func (s *PushSender) deliverJob(job *pushJob) {
	err := s.deliver(s.ctx, &job.config, job.body)
	if err == nil {
		metrics.IncPushDelivered()
		return
	}
	metrics.IncPushFailed()
	if s.ctx.Err() != nil {
		s.log.Debug("push delivery aborted by shutdown", "task", job.config.TaskID, "config", job.config.ID)
		return
	}
	s.log.Warn("push delivery failed", "task", job.config.TaskID, "config", job.config.ID, "host", webhookHost(job.config.URL), "err", err)
}

// deliver posts body until it is accepted, a permanent failure occurs, the
// attempts run out or ctx is done.
func (s *PushSender) deliver(ctx context.Context, config *a2a.PushConfig, body []byte) error {
	delay := s.policy.baseDelay
	for attempt := 1; ; attempt++ {
		retryable, err := s.postOnce(ctx, config, body)
		if err == nil {
			return nil
		}
		if !retryable || attempt >= s.policy.maxAttempts {
			return fmt.Errorf("attempt %d: %w", attempt, err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("attempt %d: %w (retry aborted: %w)", attempt, err, ctx.Err())
		case <-timer.C:
		}
		delay = min(delay*2, s.policy.maxDelay)
	}
}

// postOnce performs one POST. retryable reports whether a failure is worth
// another attempt: network errors and 5xx are, 4xx and blocked targets are not.
func (s *PushSender) postOnce(ctx context.Context, config *a2a.PushConfig, body []byte) (retryable bool, err error) {
	actx, cancel := context.WithTimeout(ctx, s.policy.perAttempt)
	defer cancel()

	req, err := http.NewRequestWithContext(actx, http.MethodPost, config.URL, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if config.Token != "" {
		req.Header.Set(pushTokenHeader, config.Token)
	}
	if config.Auth != nil && config.Auth.Credentials != "" {
		switch strings.ToLower(config.Auth.Scheme) {
		case "bearer":
			req.Header.Set("Authorization", "Bearer "+config.Auth.Credentials)
		case "basic":
			req.Header.Set("Authorization", "Basic "+config.Auth.Credentials)
		default:
			s.log.Warn("push auth scheme unsupported, sending without Authorization", "scheme", config.Auth.Scheme, "task", config.TaskID)
		}
	}

	resp, err := s.client.Do(req)
	if err != nil {
		if errors.Is(err, errBlockedPushTarget) {
			return false, err
		}
		return ctx.Err() == nil, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			s.log.Debug("push response close failed", "err", cerr)
		}
	}()
	if _, derr := io.Copy(io.Discard, io.LimitReader(resp.Body, pushResponseDrainSize)); derr != nil {
		s.log.Debug("push response drain failed", "err", derr)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode >= 500:
		return true, fmt.Errorf("webhook returned %s", resp.Status)
	default:
		return false, fmt.Errorf("webhook returned %s", resp.Status)
	}
}

// copyPushConfig detaches a config from the caller's pointers; the event
// loop may reuse them while the worker still holds the job.
func copyPushConfig(c *a2a.PushConfig) a2a.PushConfig {
	cp := *c
	if c.Auth != nil {
		auth := *c.Auth
		cp.Auth = &auth
	}
	return cp
}

// guardPushDial runs after DNS resolution, so it also covers DNS rebinding
// and every redirect hop, which a URL-string check alone cannot catch.
func guardPushDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%w: unresolved host %q", errBlockedPushTarget, host)
	}
	if isBlockedIP(ip) {
		return fmt.Errorf("%w: %s", errBlockedPushTarget, ip)
	}
	return nil
}

// isBlockedIP reports whether ip is in a range a webhook must not reach,
// covering cloud metadata endpoints and internal services.
func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// limitPushRedirects bounds the redirect chain and drops the notification
// token on cross-host hops: Go strips Authorization there but not custom headers.
func limitPushRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= pushMaxRedirects {
		return fmt.Errorf("stopped after %d redirects", pushMaxRedirects)
	}
	if len(via) > 0 && req.URL.Host != via[len(via)-1].URL.Host {
		req.Header.Del(pushTokenHeader)
	}
	return nil
}

// webhookHost returns the host of a webhook URL for logs; the full URL may
// carry credentials in its query.
func webhookHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
