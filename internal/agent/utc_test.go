package agent

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

var plusTwo = time.FixedZone("UTC+2", 2*60*60)

func zonedTime() *time.Time {
	t := time.Date(2026, 9, 15, 14, 0, 0, 0, plusTwo)
	return &t
}

func assertUTC(t *testing.T, what string, ts *time.Time) {
	t.Helper()
	if ts == nil {
		t.Fatalf("%s has no timestamp", what)
	}
	b, err := json.Marshal(ts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.Trim(string(b), `"`), "Z") {
		t.Fatalf("%s serializes as %s, want a Z suffix", what, b)
	}
	if !ts.Equal(*zonedTime()) {
		t.Fatalf("%s = %s, instant changed from %s", what, ts, zonedTime())
	}
}

// TestUTCExecutorNormalizesEvents: whatever zone an executor stamps, the
// events reaching a2a-go are UTC; Execute and Cancel are both covered.
func TestUTCExecutorNormalizesEvents(t *testing.T) {
	inner := zonedExecutor{}
	ec := &a2asrv.ExecutorContext{TaskID: "t1", ContextID: "c1"}
	u := utcExecutor{next: inner}

	for name, seq := range map[string]iter.Seq2[a2a.Event, error]{
		"execute": u.Execute(context.Background(), ec),
		"cancel":  u.Cancel(context.Background(), ec),
	} {
		count := 0
		for ev, err := range seq {
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			count++
			switch v := ev.(type) {
			case *a2a.Task:
				assertUTC(t, name+" task", v.Status.Timestamp)
			case *a2a.TaskStatusUpdateEvent:
				assertUTC(t, name+" status update", v.Status.Timestamp)
			default:
				t.Fatalf("%s: unexpected event %T", name, ev)
			}
		}
		if count == 0 {
			t.Fatalf("%s yielded nothing", name)
		}
	}
}

type zonedExecutor struct{}

func (zonedExecutor) Execute(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		task := &a2a.Task{ID: ec.TaskID, ContextID: ec.ContextID, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted, Timestamp: zonedTime()}}
		if !yield(task, nil) {
			return
		}
		yield(&a2a.TaskStatusUpdateEvent{TaskID: ec.TaskID, ContextID: ec.ContextID, Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Timestamp: zonedTime()}}, nil)
	}
}

func (zonedExecutor) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(&a2a.TaskStatusUpdateEvent{TaskID: ec.TaskID, ContextID: ec.ContextID, Status: a2a.TaskStatus{State: a2a.TaskStateCanceled, Timestamp: zonedTime()}}, nil)
	}
}

// TestTTLStoreStoresUTC: GetTask and ListTasks read back UTC timestamps even
// when a2a-go stored a zoned one.
func TestTTLStoreStoresUTC(t *testing.T) {
	s := NewTTLStore(nil, discardLog())
	defer s.Close()
	ctx := context.Background()

	task := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Timestamp: zonedTime()}}
	if _, err := s.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	assertUTC(t, "created task", got.Task.Status.Timestamp)

	task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted, Timestamp: zonedTime()}
	if _, err := s.Update(ctx, &taskstore.UpdateRequest{Task: task, PrevVersion: got.Version}); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatal(err)
	}
	assertUTC(t, "listed task", list.Tasks[0].Status.Timestamp)
}

// TestWireTimestampsUseZ: status timestamps on the wire end with Z.
func TestWireTimestampsUseZ(t *testing.T) {
	a := newTestAgent(t, "peer")
	id := completedTaskID(t, a)

	resp := rawCall(t, http.MethodPost, a.url+"/", "1.0", jsonrpcRequest(t, 6, "GetTask", map[string]any{"id": id}))
	var out struct {
		Result struct {
			Status struct {
				Timestamp string `json:"timestamp"`
			} `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp.body, &out); err != nil {
		t.Fatalf("decode %s: %v", resp.body, err)
	}
	if ts := out.Result.Status.Timestamp; ts == "" || !strings.HasSuffix(ts, "Z") {
		t.Fatalf("status.timestamp = %q, want ISO 8601 with Z", ts)
	}
}
