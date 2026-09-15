package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// fakeClock returns a clock that advances one second per call.
func fakeClock(start time.Time) func() time.Time {
	now := start
	return func() time.Time {
		now = now.Add(time.Second)
		return now
	}
}

func newTask(id, contextID string, state a2a.TaskState) *a2a.Task {
	return &a2a.Task{
		ID:        a2a.TaskID(id),
		ContextID: contextID,
		Status:    a2a.TaskStatus{State: state},
		History:   []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("q-"+id))},
		Artifacts: []*a2a.Artifact{{ID: "art-" + a2a.ArtifactID(id), Parts: a2a.ContentParts{a2a.NewTextPart("a-" + id)}}},
	}
}

func TestTTLStoreCreateGetCopies(t *testing.T) {
	s := NewTTLStore(nil, discardLog())
	defer s.Close()
	ctx := context.Background()

	orig := newTask("t1", "c1", a2a.TaskStateSubmitted)
	if v, err := s.Create(ctx, orig); err != nil || v != 1 {
		t.Fatalf("Create = %d, %v; want 1, nil", v, err)
	}
	if _, err := s.Create(ctx, orig); !errors.Is(err, taskstore.ErrTaskAlreadyExists) {
		t.Fatalf("duplicate Create = %v, want ErrTaskAlreadyExists", err)
	}

	orig.Status.State = a2a.TaskStateFailed
	got, err := s.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Task.Status.State != a2a.TaskStateSubmitted {
		t.Fatalf("stored task changed through the caller's pointer: %s", got.Task.Status.State)
	}
	got.Task.History = nil
	again, err := s.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Task.History) != 1 || again.Task.History[0].Parts[0].Text() != "q-t1" {
		t.Fatalf("stored task changed through a read copy: %+v", again.Task.History)
	}

	if _, err := s.Get(ctx, "missing"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrTaskNotFound", err)
	}
	if _, err := s.Create(ctx, nil); err == nil {
		t.Fatal("Create(nil) succeeded")
	}
}

func TestTTLStoreUpdateOCC(t *testing.T) {
	s := NewTTLStore(nil, discardLog())
	defer s.Close()
	ctx := context.Background()

	task := newTask("t1", "c1", a2a.TaskStateSubmitted)
	if _, err := s.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	task.Status.State = a2a.TaskStateWorking
	v2, err := s.Update(ctx, &taskstore.UpdateRequest{Task: task, PrevVersion: 1})
	if err != nil || v2 != 2 {
		t.Fatalf("Update = %d, %v; want 2, nil", v2, err)
	}
	if _, err := s.Update(ctx, &taskstore.UpdateRequest{Task: task, PrevVersion: 1}); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("stale Update = %v, want ErrConcurrentModification", err)
	}
	if v3, err := s.Update(ctx, &taskstore.UpdateRequest{Task: task, PrevVersion: taskstore.TaskVersionMissing}); err != nil || v3 != 3 {
		t.Fatalf("untracked Update = %d, %v; want 3, nil", v3, err)
	}
	if _, err := s.Update(ctx, &taskstore.UpdateRequest{Task: newTask("nope", "c1", a2a.TaskStateWorking)}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("Update(missing) = %v, want ErrTaskNotFound", err)
	}
}

func TestTTLStoreListFiltersAndPaging(t *testing.T) {
	s := NewTTLStore(nil, discardLog())
	defer s.Close()
	s.now = fakeClock(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()

	seed := []struct {
		id, ctxID string
		state     a2a.TaskState
	}{
		{"a1", "ctx-a", a2a.TaskStateCompleted},
		{"a2", "ctx-a", a2a.TaskStateWorking},
		{"a3", "ctx-a", a2a.TaskStateCompleted},
		{"b1", "ctx-b", a2a.TaskStateCompleted},
		{"b2", "ctx-b", a2a.TaskStateWorking},
	}
	for _, sd := range seed {
		if _, err := s.Create(ctx, newTask(sd.id, sd.ctxID, sd.state)); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.List(ctx, &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if all.TotalSize != 5 || len(all.Tasks) != 5 || all.Tasks[0].ID != "b2" || all.Tasks[4].ID != "a1" {
		t.Fatalf("List all = total %d, order %v; want 5 newest first", all.TotalSize, taskIDs(all.Tasks))
	}
	if all.Tasks[0].Artifacts != nil {
		t.Error("artifacts returned without IncludeArtifacts")
	}

	byCtx, err := s.List(ctx, &a2a.ListTasksRequest{ContextID: "ctx-a"})
	if err != nil || byCtx.TotalSize != 3 {
		t.Fatalf("List by context = %+v, %v; want 3", byCtx, err)
	}
	byState, err := s.List(ctx, &a2a.ListTasksRequest{Status: a2a.TaskStateWorking})
	if err != nil || byState.TotalSize != 2 {
		t.Fatalf("List by status = %+v, %v; want 2", byState, err)
	}

	page1, err := s.List(ctx, &a2a.ListTasksRequest{PageSize: 2, IncludeArtifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Tasks) != 2 || page1.NextPageToken == "" || page1.Tasks[0].Artifacts == nil {
		t.Fatalf("page1 = %v token %q", taskIDs(page1.Tasks), page1.NextPageToken)
	}
	var seen []a2a.TaskID
	seen = append(seen, taskIDs(page1.Tasks)...)
	token := page1.NextPageToken
	for token != "" {
		page, err := s.List(ctx, &a2a.ListTasksRequest{PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, taskIDs(page.Tasks)...)
		token = page.NextPageToken
	}
	if fmt.Sprint(seen) != fmt.Sprint(taskIDs(all.Tasks)) {
		t.Fatalf("paged order = %v, want %v", seen, taskIDs(all.Tasks))
	}

	zero := 0
	noHistory, err := s.List(ctx, &a2a.ListTasksRequest{HistoryLength: &zero})
	if err != nil || len(noHistory.Tasks[0].History) != 0 {
		t.Fatalf("HistoryLength 0 = %+v, %v", noHistory.Tasks[0].History, err)
	}

	for _, size := range []int{-1, maxListPageSize + 1} {
		if _, err := s.List(ctx, &a2a.ListTasksRequest{PageSize: size}); !errors.Is(err, a2a.ErrInvalidRequest) {
			t.Errorf("PageSize %d = %v, want ErrInvalidRequest", size, err)
		}
	}
	if _, err := s.List(ctx, &a2a.ListTasksRequest{PageToken: "%%%"}); err == nil {
		t.Error("malformed page token accepted")
	}
}

func TestTTLStoreEvictTerminal(t *testing.T) {
	pushConfigs := push.NewInMemoryStore()
	s := NewTTLStore(pushConfigs, discardLog())
	defer s.Close()
	ctx := context.Background()
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	s.now = func() time.Time { return start }
	for _, task := range []*a2a.Task{
		newTask("old-done", "c", a2a.TaskStateCompleted),
		newTask("old-working", "c", a2a.TaskStateWorking),
	} {
		if _, err := s.Create(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pushConfigs.Save(ctx, "old-done", &a2a.PushConfig{URL: "http://hook.invalid/"}); err != nil {
		t.Fatal(err)
	}
	now := start.Add(2 * terminalTaskTTL)
	s.now = func() time.Time { return now }
	if _, err := s.Create(ctx, newTask("fresh-done", "c", a2a.TaskStateCompleted)); err != nil {
		t.Fatal(err)
	}

	if got := s.EvictTerminal(now); got != 1 {
		t.Fatalf("EvictTerminal = %d, want 1", got)
	}
	if _, err := s.Get(ctx, "old-done"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("old terminal task survived: %v", err)
	}
	for _, id := range []a2a.TaskID{"old-working", "fresh-done"} {
		if _, err := s.Get(ctx, id); err != nil {
			t.Errorf("%s evicted: %v", id, err)
		}
	}
	configs, err := pushConfigs.List(ctx, "old-done")
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 0 {
		t.Errorf("push configs survived eviction: %d", len(configs))
	}
}

func taskIDs(tasks []*a2a.Task) []a2a.TaskID {
	ids := make([]a2a.TaskID, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	return ids
}
