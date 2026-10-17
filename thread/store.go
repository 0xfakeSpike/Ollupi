package thread

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Change is what one write stores, in one transaction: the thread and the
// turns and items that changed. Each is inserted or, when its id exists,
// rewritten; a turn or item never changes its thread or seq.
type Change struct {
	Thread Thread
	Turns  []Turn
	Items  []Item
}

// Persistence stores threads, turns, and items. Implemented by *storage.DB.
type Persistence interface {
	// Save stores the changes in one transaction.
	Save(changes ...Change) error
	// Load returns every thread with its turns and items in seq order.
	Load() ([]View, error)
}

type entry struct {
	thread Thread
	turns  []Turn
	items  []Item
}

// Store is the in-memory index of every thread, written through to the
// database. Every write is persisted before memory changes, and every value
// handed out is a copy.
type Store struct {
	mu      sync.RWMutex
	db      Persistence
	entries map[string]*entry
}

// Open loads every thread. Turns that were in progress when the process
// stopped stay in progress: the thread manager recovers them (Recover) and
// runs them again.
func Open(db Persistence) (*Store, error) {
	s := &Store{db: db, entries: make(map[string]*entry)}
	views, err := db.Load()
	if err != nil {
		return nil, fmt.Errorf("thread store: load: %w", err)
	}
	for _, v := range views {
		s.entries[v.Thread.ID] = &entry{thread: v.Thread, turns: v.Turns, items: v.Items}
	}
	return s, nil
}

// ActiveThreads returns the threads with a turn in progress, the oldest
// turn first.
func (s *Store) ActiveThreads() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type active struct {
		id      string
		started time.Time
	}
	var list []active
	for id, e := range s.entries {
		if turn, ok := e.activeTurn(); ok {
			list = append(list, active{id, turn.StartedAt})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].started.Before(list[j].started) })
	ids := make([]string, len(list))
	for i, a := range list {
		ids[i] = a.id
	}
	return ids
}

// Recover closes what the thread's turn in progress lost when the process
// stopped, so the turn can run again from its items: text that was
// streaming and a compaction under way fail, and so does the call that was
// being handled when it is a registry tool call, because it may or may not
// have taken effect. Calls not reached yet, and requests and subagents the
// turn waits for, are kept. It returns the closed items.
func (s *Store) Recover(threadID string) ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.entry(threadID)
	if err != nil {
		return nil, err
	}
	active, ok := e.activeTurn()
	if !ok {
		return nil, nil
	}
	now := time.Now().UTC()
	next := e.copy()
	var closed []Item
	// handling is true until the first call the Runner had not finished:
	// calls are handled in order, so only that one may have been running.
	handling := true
	for i, it := range next.items {
		if it.TurnID != active.ID || it.Status != ItemInProgress {
			continue
		}
		switch {
		case it.Type == ItemReasoning || it.Type == ItemAgentMessage || it.Type == ItemContextCompaction:
			it.Error = "the app stopped before this finished"
		case it.IsCall() && handling:
			if it.WaitsOnAgent() {
				continue
			}
			handling = false
			declined := it.Approval != nil && it.Approval.Decision == DecisionDecline
			if it.Type != ItemMcpToolCall || it.IsPending() || declined {
				continue
			}
			it.Error = "the app stopped while this call was being handled; it may or may not have taken effect, so check before repeating it"
		default:
			continue
		}
		it.Status, it.CompletedAt = ItemFailed, &now
		next.items[i] = it
		closed = append(closed, it)
	}
	if len(closed) == 0 {
		return nil, nil
	}
	if err := s.commit(e, next, nil, closed); err != nil {
		return nil, err
	}
	return clone(closed), nil
}

// Create stores a new thread.
func (s *Store) Create(t Thread) (Thread, error) {
	now := time.Now().UTC()
	t.ID = uuid.NewString()
	t.CreatedAt, t.UpdatedAt = now, now
	s.mu.Lock()
	defer s.mu.Unlock()
	e := &entry{thread: t}
	if err := s.write(e, nil, nil); err != nil {
		return Thread{}, err
	}
	s.entries[t.ID] = e
	return clone(t), nil
}

// Get returns one thread.
func (s *Store) Get(id string) (Thread, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[strings.TrimSpace(id)]
	if !ok {
		return Thread{}, false
	}
	return clone(e.thread), true
}

// Read returns a thread with its turns and items.
func (s *Store) Read(id string) (View, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[strings.TrimSpace(id)]
	if !ok {
		return View{}, false
	}
	return clone(e.view()), true
}

// Filter selects threads for List. Empty fields match everything.
type Filter struct {
	CronJobID string
	// WorkspaceID nil matches all; an empty value selects general conversations.
	WorkspaceID *string
	Limit       int
}

// List returns threads with their status, most recently updated first.
// Subagent threads are not listed; they show in their parent.
func (s *Store) List(f Filter) []Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Summary, 0, len(s.entries))
	for _, e := range s.entries {
		if e.thread.Source == SourceSubagent || (f.CronJobID != "" && e.thread.CronJobID != f.CronJobID) {
			continue
		}
		if f.WorkspaceID != nil && e.thread.WorkspaceID != *f.WorkspaceID {
			continue
		}
		out = append(out, Summary{Thread: e.thread, Status: s.statusLocked(e)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return clone(out)
}

// Status returns a thread's status. A thread whose turn waits for its
// subagents takes the status of a subagent that waits for the user, so the
// user sees that the conversation needs them.
func (s *Store) Status(id string) (Status, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[id]
	if !ok {
		return "", false
	}
	return s.statusLocked(e), true
}

func (s *Store) statusLocked(e *entry) Status {
	v := e.view()
	status := v.Status()
	if status != StatusActive {
		return status
	}
	for _, call := range v.AgentCalls() {
		for _, id := range call.ReceiverThreadIDs {
			if child, ok := s.entries[id]; ok {
				if cs := child.view().Status(); cs == StatusWaitingOnApproval || cs == StatusWaitingOnUserInput {
					return cs
				}
			}
		}
	}
	return status
}

// BeginTurn stores a new in-progress turn together with its input items
// and returns the stored turn. A thread has at most one turn in progress.
func (s *Store) BeginTurn(threadID string, turn Turn, input []Item) (Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.entry(threadID)
	if err != nil {
		return Turn{}, err
	}
	if active, ok := e.activeTurn(); ok {
		return Turn{}, fmt.Errorf("thread store: thread %s already has turn %s in progress", threadID, active.ID)
	}
	if turn.ID == "" || turn.Status != TurnInProgress {
		return Turn{}, fmt.Errorf("thread store: a new turn needs an id and status inProgress")
	}
	turn.Seq = len(e.turns) + 1
	next := e.copy()
	next.turns = append(next.turns, turn)
	if err := next.upsert(input); err != nil {
		return Turn{}, err
	}
	next.thread.UpdatedAt = time.Now().UTC()
	stored := make([]Item, 0, len(input))
	for _, it := range input {
		if it.Type == ItemUserMessage && next.thread.Preview == "" {
			next.thread.Preview = clip(firstLine(it.Text), 80)
		}
		stored = append(stored, next.items[next.index(it.ID)])
	}
	if err := s.commit(e, next, []Turn{turn}, stored); err != nil {
		return Turn{}, err
	}
	return clone(turn), nil
}

// Record stores one item of the thread's turn in progress: a new item is
// appended, an existing one rewritten in place. It returns the stored item
// and whether it is new.
func (s *Store) Record(threadID string, item Item) (Item, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.entry(threadID)
	if err != nil {
		return Item{}, false, err
	}
	active, ok := e.activeTurn()
	if !ok || active.ID != item.TurnID {
		return Item{}, false, fmt.Errorf("thread store: item %s belongs to turn %s, which is not in progress", item.ID, item.TurnID)
	}
	isNew := e.index(item.ID) < 0
	next := e.copy()
	if err := next.upsert([]Item{item}); err != nil {
		return Item{}, false, err
	}
	stored := next.items[next.index(item.ID)]
	if err := s.commit(e, next, nil, []Item{stored}); err != nil {
		return Item{}, false, err
	}
	return clone(stored), isNew, nil
}

// AddUsage adds token usage to the thread's turn in progress.
func (s *Store) AddUsage(threadID, turnID string, u Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.entry(threadID)
	if err != nil {
		return err
	}
	active, ok := e.activeTurn()
	if !ok || active.ID != turnID {
		return fmt.Errorf("thread store: turn %s is not in progress", turnID)
	}
	if u == (Usage{}) {
		return nil
	}
	active.Usage.Add(u)
	next := e.copy()
	next.turns[len(next.turns)-1] = active
	return s.commit(e, next, []Turn{active}, nil)
}

// CompleteTurn ends the thread's turn in progress with turn's status,
// error, and usage. Items of the turn still in progress are failed; they are
// returned.
func (s *Store) CompleteTurn(threadID string, turn Turn) ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.entry(threadID)
	if err != nil {
		return nil, err
	}
	active, ok := e.activeTurn()
	if !ok || active.ID != turn.ID {
		return nil, fmt.Errorf("thread store: turn %s is not in progress", turn.ID)
	}
	return s.completeTurnLocked(e, turn)
}

// CompleteSubagentTurn ends a subagent thread's turn as CompleteTurn does
// and, in the same transaction, records report: the Agent call item of the
// parent thread's turn in progress that launched it.
func (s *Store) CompleteSubagentTurn(threadID string, turn Turn, report Item) (closed []Item, stored Item, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.entry(threadID)
	if err != nil {
		return nil, Item{}, err
	}
	if active, ok := e.activeTurn(); !ok || active.ID != turn.ID {
		return nil, Item{}, fmt.Errorf("thread store: turn %s is not in progress", turn.ID)
	}
	p, err := s.entry(e.thread.ParentThreadID)
	if err != nil {
		return nil, Item{}, err
	}
	if active, ok := p.activeTurn(); !ok || active.ID != report.TurnID || report.ID != e.thread.ParentItemID {
		return nil, Item{}, fmt.Errorf("thread store: item %s is not the Agent call of thread %s's turn in progress", report.ID, p.thread.ID)
	}
	next, closed, err := completedTurn(e, turn)
	if err != nil {
		return nil, Item{}, err
	}
	parent := p.copy()
	if err := parent.upsert([]Item{report}); err != nil {
		return nil, Item{}, err
	}
	stored = parent.items[parent.index(report.ID)]
	if err := s.db.Save(Change{Thread: next.thread, Turns: []Turn{next.turns[len(next.turns)-1]}, Items: closed}, Change{Thread: parent.thread, Items: []Item{stored}}); err != nil {
		return nil, Item{}, fmt.Errorf("thread store: write threads %s and %s: %w", e.thread.ID, p.thread.ID, err)
	}
	*e, *p = *next, *parent
	return clone(closed), clone(stored), nil
}

func (s *Store) completeTurnLocked(e *entry, turn Turn) ([]Item, error) {
	next, closed, err := completedTurn(e, turn)
	if err != nil {
		return nil, err
	}
	if err := s.commit(e, next, []Turn{next.turns[len(next.turns)-1]}, closed); err != nil {
		return nil, err
	}
	return clone(closed), nil
}

// completedTurn is e with its turn in progress ended as turn says, and the
// items of the turn still in progress failed (also returned).
func completedTurn(e *entry, turn Turn) (*entry, []Item, error) {
	if turn.Status == TurnInProgress {
		return nil, nil, fmt.Errorf("thread store: turn %s must end with a final status", turn.ID)
	}
	previous := e.turns[len(e.turns)-1]
	if !reflect.DeepEqual(previous.ExecutionContext, turn.ExecutionContext) {
		return nil, nil, fmt.Errorf("thread store: turn %s cannot change its execution context", turn.ID)
	}
	now := time.Now().UTC()
	turn.CompletedAt = &now
	next := e.copy()
	next.turns[len(next.turns)-1] = turn
	next.thread.UpdatedAt = now
	var closed []Item
	for i, it := range next.items {
		if it.TurnID != turn.ID || it.Status != ItemInProgress {
			continue
		}
		it.Status = ItemFailed
		it.Error = fmt.Sprintf("the turn ended (%s) before this item finished", turn.Status)
		it.CompletedAt = &now
		next.items[i] = it
		closed = append(closed, it)
	}
	return next, closed, nil
}

func (s *Store) entry(id string) (*entry, error) {
	e, ok := s.entries[id]
	if !ok {
		return nil, fmt.Errorf("thread store: thread %s not found", id)
	}
	return e, nil
}

// commit persists the changed rows of next, then replaces e's contents.
func (s *Store) commit(e, next *entry, turns []Turn, items []Item) error {
	if err := s.write(next, turns, items); err != nil {
		return err
	}
	*e = *next
	return nil
}

func (s *Store) write(e *entry, turns []Turn, items []Item) error {
	if err := s.db.Save(Change{Thread: e.thread, Turns: turns, Items: items}); err != nil {
		return fmt.Errorf("thread store: write thread %s: %w", e.thread.ID, err)
	}
	return nil
}

func (e *entry) view() View {
	return View{Thread: e.thread, Turns: append([]Turn{}, e.turns...), Items: append([]Item{}, e.items...)}
}

func (e *entry) activeTurn() (Turn, bool) {
	if n := len(e.turns); n > 0 && e.turns[n-1].Status == TurnInProgress {
		return e.turns[n-1], true
	}
	return Turn{}, false
}

func (e *entry) index(itemID string) int {
	for i := len(e.items) - 1; i >= 0; i-- {
		if e.items[i].ID == itemID {
			return i
		}
	}
	return -1
}

// upsert appends new items and rewrites existing ones, which must keep
// their type and turn.
func (e *entry) upsert(items []Item) error {
	for _, it := range items {
		if it.ID == "" || it.TurnID == "" || it.Type == "" || it.Status == "" {
			return fmt.Errorf("thread store: item needs an id, turn, type, and status")
		}
		if i := e.index(it.ID); i >= 0 {
			prev := e.items[i]
			if prev.Type != it.Type || prev.TurnID != it.TurnID {
				return fmt.Errorf("thread store: item %s cannot change its type or turn", it.ID)
			}
			if prev.PreparedExecution != nil && (!reflect.DeepEqual(prev.PreparedExecution, it.PreparedExecution) || prev.Arguments != it.Arguments || prev.Tool != it.Tool || prev.Server != it.Server) {
				return fmt.Errorf("thread store: item %s cannot change its prepared execution or tool arguments", it.ID)
			}
			it.Seq = prev.Seq
			e.items[i] = it
			continue
		}
		it.Seq = len(e.items) + 1
		e.items = append(e.items, it)
	}
	return nil
}

func (e *entry) copy() *entry {
	return &entry{thread: e.thread, turns: append([]Turn(nil), e.turns...), items: append([]Item(nil), e.items...)}
}

func encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("thread store: encode %T: %v", v, err))
	}
	return b
}

// clone deep-copies a stored value, so callers never share its pointers.
func clone[T any](v T) T {
	var out T
	if err := json.Unmarshal(encode(v), &out); err != nil {
		panic(fmt.Sprintf("thread store: clone %T: %v", v, err))
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func clip(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes])
}
