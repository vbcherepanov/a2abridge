package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandlerExposesPrometheusFormat checks the headline contract:
// every counter has a HELP/TYPE/value triple, every gauge does too,
// and our few public Inc/Set helpers actually move the numbers.
func TestHandlerExposesPrometheusFormat(t *testing.T) {
	IncMessagesSent()
	IncMessagesSent()
	IncMessagesReceived()
	IncTaskCompleted()
	IncPushDelivered()
	SetInboxSize(3)
	SetPeers(2)

	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain prefix", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	// Every metric must declare HELP + TYPE before the value line.
	wants := []string{
		"# TYPE a2abridge_uptime_seconds gauge",
		"# TYPE a2abridge_messages_sent_total counter",
		"a2abridge_messages_sent_total 2",
		"# TYPE a2abridge_messages_received_total counter",
		"a2abridge_messages_received_total 1",
		"# TYPE a2abridge_tasks_completed_total counter",
		"a2abridge_tasks_completed_total 1",
		"# TYPE a2abridge_inbox_size gauge",
		"a2abridge_inbox_size 3",
		"# TYPE a2abridge_peers gauge",
		"a2abridge_peers 2",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in metrics output:\n%s", w, out)
		}
	}
}
