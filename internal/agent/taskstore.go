package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// Task store knobs. List paging mirrors a2a-go's taskstore.InMemory.
const (
	// terminalTaskTTL keeps terminal tasks around for a grace period so late
	// GetTask / resubscribe calls still resolve, then evicts them to keep a
	// long-lived bridge bounded.
	terminalTaskTTL = 30 * time.Minute

	defaultListPageSize      = 50
	maxListPageSize          = 100
	defaultListHistoryLength = 100
	pageTokenSeparator       = "_"
)

var errNilTask = errors.New("task is required")

type ttlEntry struct {
	task        *a2a.Task
	version     taskstore.TaskVersion
	lastUpdated time.Time
}

// TTLStore is an in-memory taskstore.Store for a single-user bridge: no
// ownership checks, optimistic concurrency on Update, deep copies on every
// read and write, and eviction of terminal tasks after terminalTaskTTL.
type TTLStore struct {
	mu          sync.RWMutex
	tasks       map[a2a.TaskID]*ttlEntry
	pushConfigs push.ConfigStore
	log         *slog.Logger
	now         func() time.Time

	stop      chan struct{}
	closeOnce sync.Once
}

var _ taskstore.Store = (*TTLStore)(nil)

// NewTTLStore returns an empty store with its eviction janitor running.
// Push configs of evicted tasks are removed from pushConfigs when non-nil.
func NewTTLStore(pushConfigs push.ConfigStore, log *slog.Logger) *TTLStore {
	s := &TTLStore{
		tasks:       map[a2a.TaskID]*ttlEntry{},
		pushConfigs: pushConfigs,
		log:         log,
		now:         time.Now,
		stop:        make(chan struct{}),
	}
	go s.janitor()
	return s
}

// Close stops the janitor. Safe to call multiple times.
func (s *TTLStore) Close() {
	s.closeOnce.Do(func() { close(s.stop) })
}

func (s *TTLStore) janitor() {
	t := time.NewTicker(janitorInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.EvictTerminal(s.now())
		}
	}
}

// Create implements taskstore.Store.
func (s *TTLStore) Create(_ context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	if task == nil {
		return taskstore.TaskVersionMissing, errNilTask
	}
	cp, err := cloneTask(task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	statusInUTC(&cp.Status)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[task.ID]; ok {
		return taskstore.TaskVersionMissing, taskstore.ErrTaskAlreadyExists
	}
	const firstVersion taskstore.TaskVersion = 1
	s.tasks[task.ID] = &ttlEntry{task: cp, version: firstVersion, lastUpdated: s.now()}
	return firstVersion, nil
}

// Update implements taskstore.Store.
func (s *TTLStore) Update(_ context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if req == nil || req.Task == nil {
		return taskstore.TaskVersionMissing, errNilTask
	}
	cp, err := cloneTask(req.Task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	statusInUTC(&cp.Status)
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.tasks[req.Task.ID]
	if !ok {
		return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
	}
	if req.PrevVersion != taskstore.TaskVersionMissing && stored.version != req.PrevVersion {
		return taskstore.TaskVersionMissing, taskstore.ErrConcurrentModification
	}
	version := stored.version + 1
	s.tasks[req.Task.ID] = &ttlEntry{task: cp, version: version, lastUpdated: s.now()}
	return version, nil
}

// Get implements taskstore.Store.
func (s *TTLStore) Get(_ context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	s.mu.RLock()
	stored, ok := s.tasks[taskID]
	s.mu.RUnlock()
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	// Entries are replaced, never mutated, so copying outside the lock is safe.
	cp, err := cloneTask(stored.task)
	if err != nil {
		return nil, err
	}
	return &taskstore.StoredTask{Task: cp, Version: stored.version}, nil
}

// List implements taskstore.Store with a2a-go InMemory semantics minus the
// per-user filter: a bridge serves a single local user.
func (s *TTLStore) List(_ context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	if req == nil {
		req = &a2a.ListTasksRequest{}
	}
	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = defaultListPageSize
	} else if pageSize < 1 || pageSize > maxListPageSize {
		return nil, fmt.Errorf("page size must be between 1 and %d inclusive, got %d: %w", maxListPageSize, pageSize, a2a.ErrInvalidRequest)
	}

	s.mu.RLock()
	filtered := make([]*ttlEntry, 0, len(s.tasks))
	for _, e := range s.tasks {
		if matchesListFilter(e, req) {
			filtered = append(filtered, e)
		}
	}
	s.mu.RUnlock()

	slices.SortFunc(filtered, func(a, b *ttlEntry) int {
		if c := b.lastUpdated.Compare(a.lastUpdated); c != 0 {
			return c
		}
		return strings.Compare(string(b.task.ID), string(a.task.ID))
	})

	page := filtered
	if req.PageToken != "" {
		cursorTime, cursorID, err := decodePageToken(req.PageToken)
		if err != nil {
			return nil, err
		}
		start := sort.Search(len(filtered), func(i int) bool {
			if c := filtered[i].lastUpdated.Compare(cursorTime); c != 0 {
				return c < 0
			}
			return string(filtered[i].task.ID) < string(cursorID)
		})
		page = filtered[start:]
	}

	nextPageToken := ""
	if len(page) > pageSize {
		last := page[pageSize-1]
		nextPageToken = encodePageToken(last.lastUpdated, last.task.ID)
		page = page[:pageSize]
	}

	historyLength := defaultListHistoryLength
	if req.HistoryLength != nil {
		historyLength = *req.HistoryLength
	}
	tasks := make([]*a2a.Task, 0, len(page))
	for _, e := range page {
		cp, err := cloneTask(e.task)
		if err != nil {
			return nil, err
		}
		if historyLength <= 0 {
			cp.History = []*a2a.Message{}
		} else if len(cp.History) > historyLength {
			cp.History = cp.History[len(cp.History)-historyLength:]
		}
		if !req.IncludeArtifacts {
			cp.Artifacts = nil
		}
		tasks = append(tasks, cp)
	}

	return &a2a.ListTasksResponse{
		Tasks:         tasks,
		TotalSize:     len(filtered),
		PageSize:      len(tasks),
		NextPageToken: nextPageToken,
	}, nil
}

// EvictTerminal removes terminal tasks not updated for terminalTaskTTL
// before now, together with their push configs. Non-terminal tasks are never
// evicted — they may still receive events. Returns the number of evicted tasks.
func (s *TTLStore) EvictTerminal(now time.Time) int {
	cutoff := now.Add(-terminalTaskTTL)
	s.mu.Lock()
	var evicted []a2a.TaskID
	for id, e := range s.tasks {
		if e.task.Status.State.Terminal() && e.lastUpdated.Before(cutoff) {
			delete(s.tasks, id)
			evicted = append(evicted, id)
		}
	}
	s.mu.Unlock()

	if s.pushConfigs != nil {
		for _, id := range evicted {
			if err := s.pushConfigs.DeleteAll(context.Background(), id); err != nil {
				s.log.Warn("push config cleanup failed", "task", id, "err", err)
			}
		}
	}
	if len(evicted) > 0 {
		s.log.Info("evicted terminal tasks", "count", len(evicted), "ttl", terminalTaskTTL)
	}
	return len(evicted)
}

func matchesListFilter(e *ttlEntry, req *a2a.ListTasksRequest) bool {
	if req.ContextID != "" && e.task.ContextID != req.ContextID {
		return false
	}
	if req.Status != a2a.TaskStateUnspecified && e.task.Status.State != req.Status {
		return false
	}
	if req.StatusTimestampAfter != nil && e.task.Status.Timestamp != nil &&
		e.task.Status.Timestamp.Before(*req.StatusTimestampAfter) {
		return false
	}
	return true
}

// cloneTask deep-copies a task through its JSON wire form. Plain Unmarshal
// (no UseNumber) on purpose: metadata numbers must stay float64, exactly as
// they arrive off the wire, because a2a-go gob-copies tasks and gob cannot
// encode json.Number inside map[string]any.
func cloneTask(t *a2a.Task) (*a2a.Task, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("encode task %s: %w", t.ID, err)
	}
	var cp a2a.Task
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, fmt.Errorf("decode task %s: %w", t.ID, err)
	}
	return &cp, nil
}

func encodePageToken(updated time.Time, id a2a.TaskID) string {
	return base64.URLEncoding.EncodeToString([]byte(updated.Format(time.RFC3339Nano) + pageTokenSeparator + string(id)))
}

func decodePageToken(token string) (time.Time, a2a.TaskID, error) {
	raw, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("page token: %w", a2a.ErrInvalidParams)
	}
	ts, id, ok := strings.Cut(string(raw), pageTokenSeparator)
	if !ok || id == "" {
		return time.Time{}, "", fmt.Errorf("page token: %w", a2a.ErrInvalidParams)
	}
	updated, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("page token time: %w", a2a.ErrInvalidParams)
	}
	return updated, a2a.TaskID(id), nil
}
