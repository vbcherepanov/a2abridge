// Package metrics exposes a tiny Prometheus-text /metrics endpoint
// without pulling in the official prometheus/client_golang dependency
// (which is ~6 MB of transitive code we don't otherwise use). Everything
// the bridge cares about — peer count, inbox size, outbound task
// pendency — fits in a few atomic counters and gauges.
//
// The format is the standard text exposition format described at
// https://prometheus.io/docs/instrumenting/exposition_formats/.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// Counters are monotonic; gauges go up and down. We keep them as
// atomics so multiple goroutines (HTTP handlers, hooks, the SSE poll
// loop) can update without contention.
var (
	startedAt = time.Now()

	messagesSent     atomic.Int64 // a2a.SendMessage: outbound
	messagesReceived atomic.Int64 // a2a.SendMessage: inbound
	tasksCompleted   atomic.Int64 // tasks reaching terminal state COMPLETED
	tasksFailed      atomic.Int64 // tasks reaching terminal state FAILED/CANCELED/REJECTED
	pushDelivered    atomic.Int64 // webhook deliveries that returned 2xx
	pushFailed       atomic.Int64 // webhook deliveries that exhausted retries

	currentInboxSize atomic.Int64 // gauge — set, not increment
	currentPeers     atomic.Int64 // gauge — set, not increment
)

// Public increment helpers — call from the relevant code paths.
func IncMessagesSent()     { messagesSent.Add(1) }
func IncMessagesReceived() { messagesReceived.Add(1) }
func IncTaskCompleted()    { tasksCompleted.Add(1) }
func IncTaskFailed()       { tasksFailed.Add(1) }
func IncPushDelivered()    { pushDelivered.Add(1) }
func IncPushFailed()       { pushFailed.Add(1) }

// Public gauge setters.
func SetInboxSize(n int) { currentInboxSize.Store(int64(n)) }
func SetPeers(n int)     { currentPeers.Store(int64(n)) }

// Handler returns an http.Handler that emits Prometheus exposition.
// Mount it at /metrics on the directory daemon and (optionally) on each
// bridge.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		emit(w)
	})
}

func emit(w io.Writer) {
	uptime := time.Since(startedAt).Seconds()

	fmt.Fprintln(w, "# HELP a2abridge_uptime_seconds Seconds since this bridge started.")
	fmt.Fprintln(w, "# TYPE a2abridge_uptime_seconds gauge")
	fmt.Fprintf(w, "a2abridge_uptime_seconds %g\n", uptime)

	counter(w, "a2abridge_messages_sent_total",
		"Outbound a2a.SendMessage calls.", messagesSent.Load())
	counter(w, "a2abridge_messages_received_total",
		"Inbound a2a.SendMessage calls.", messagesReceived.Load())
	counter(w, "a2abridge_tasks_completed_total",
		"Tasks that reached COMPLETED state.", tasksCompleted.Load())
	counter(w, "a2abridge_tasks_failed_total",
		"Tasks that reached FAILED / CANCELED / REJECTED.", tasksFailed.Load())
	counter(w, "a2abridge_push_delivered_total",
		"Push-notification webhooks that returned 2xx.", pushDelivered.Load())
	counter(w, "a2abridge_push_failed_total",
		"Push-notification webhooks that exhausted the retry budget.", pushFailed.Load())

	gauge(w, "a2abridge_inbox_size",
		"Current pending messages in this bridge's inbox.", currentInboxSize.Load())
	gauge(w, "a2abridge_peers",
		"Current peers registered with the directory.", currentPeers.Load())
}

func counter(w io.Writer, name, help string, v int64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
}

func gauge(w io.Writer, name, help string, v int64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, v)
}
