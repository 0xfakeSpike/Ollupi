package agent

import (
	"context"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/runtime"
	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

const workers = 4

type slot struct {
	cancel  context.CancelFunc
	steered bool
}

// work is one queue element: the smallest identifiers a consumer needs. Every
// relational detail (thread, workspace, items) is read from SQL at consumption
// time; the only in-memory objects involved come from the agent itself.
type work struct {
	threadID string
	turnID   string
	ctx      context.Context
}

// scheduler owns the shared turn queue, the per-thread cancellation slots and
// the persistent consumers. It is the only place that decides whether a turn
// continues, waits, or ends.
type scheduler struct {
	a   *Agent
	ctx context.Context

	mu         sync.Mutex
	cond       *sync.Cond
	slots      map[string]*slot
	queue      []work
	background sync.WaitGroup
	workers    sync.WaitGroup
}

func newScheduler(a *Agent, ctx context.Context) *scheduler {
	s := &scheduler{a: a, ctx: ctx, slots: map[string]*slot{}}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *scheduler) start() {
	for range workers {
		s.workers.Add(1)
		go s.workerLoop()
	}
	s.workers.Add(1)
	go s.keepTime()
}

func (s *scheduler) wait() {
	s.background.Wait()
	if s.ctx.Err() != nil {
		s.workers.Wait()
	}
}

func (s *scheduler) enqueue(threadID, turnID string) {
	if s.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.slots[threadID] = &slot{cancel: cancel}
	s.queue = append(s.queue, work{threadID: threadID, turnID: turnID, ctx: ctx})
	s.cond.Signal()
}

func (s *scheduler) queued(threadID string) bool {
	return slices.ContainsFunc(s.queue, func(w work) bool { return w.threadID == threadID })
}

// nextWork blocks a consumer until a turn is queued or the agent shuts down.
func (s *scheduler) nextWork() (work, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.queue) == 0 && s.ctx.Err() == nil {
		s.cond.Wait()
	}
	if s.ctx.Err() != nil {
		return work{}, false
	}
	w := s.queue[0]
	s.queue = s.queue[1:]
	return w, true
}

func (s *scheduler) workerLoop() {
	defer s.workers.Done()
	for {
		w, ok := s.nextWork()
		if !ok {
			return
		}
		s.background.Add(1)
		s.run(w)
		s.background.Done()
	}
}

func (s *scheduler) run(w work) {
	view, err := s.a.ReadThread(w.threadID)
	if err != nil {
		log.Printf("agent: thread %s cannot be read while turn %s is queued: %v", w.threadID, w.turnID, err)
		if turn, err := s.a.DB.Turn(w.turnID); err == nil {
			turn.Status, turn.Error = repo.TurnFailed, "the thread could not be read from storage"
			s.mu.Lock()
			s.release(w.threadID)
			s.endTurn(turn)
			s.mu.Unlock()
		}
		return
	}
	turn, ok := findTurn(view, w.turnID)
	if !ok || turn.Status != repo.TurnInProgress {
		s.mu.Lock()
		s.release(w.threadID)
		s.mu.Unlock()
		return
	}
	outcome, usage, err := s.a.step(w.ctx, view, turn)
	if err := s.a.addUsage(w.threadID, turn.ID, usage); err != nil {
		log.Printf("agent: thread %s: record usage of turn %s: %v", w.threadID, turn.ID, err)
	}
	interrupted := w.ctx.Err() != nil
	s.mu.Lock()
	defer s.mu.Unlock()
	s.afterStep(w.threadID, outcome, err, interrupted)
}

// release cancels and forgets the slot of a thread whose turn is no longer
// running on this work item.
func (s *scheduler) release(threadID string) {
	if sl, exists := s.slots[threadID]; exists {
		sl.cancel()
		delete(s.slots, threadID)
	}
}

func (s *scheduler) afterStep(threadID string, outcome runtime.Outcome, err error, interrupted bool) {
	sl, ok := s.slots[threadID]
	if !ok {
		return
	}
	s.release(threadID)
	view, err := s.a.ReadThread(threadID)
	if err != nil {
		log.Printf("agent: thread %s cannot be read after its step: %v", threadID, err)
		return
	}
	turn, ok := view.ActiveTurn()
	if !ok {
		return
	}
	switch {
	case s.ctx.Err() != nil && (err != nil || interrupted):
		log.Printf("agent: thread %s turn %s left in progress at shutdown: %v", threadID, turn.ID, err)
	case interrupted:
		turn.Status, turn.Error = repo.TurnInterrupted, "interrupted by the user"
		s.endTurn(turn)
	case err != nil:
		turn.Status, turn.Error = repo.TurnFailed, err.Error()
		log.Printf("agent: thread %s turn %s failed: %v", threadID, turn.ID, err)
		s.endTurn(turn)
	case outcome == runtime.Continue || (outcome == runtime.Done && sl.steered):
		s.enqueue(threadID, turn.ID)
	case outcome == runtime.Wait:
		s.launchSubagents(threadID)
		view, err = s.a.ReadThread(threadID)
		if err != nil {
			log.Printf("agent: thread %s cannot be read while waiting: %v", threadID, err)
			return
		}
		s.a.emitRequests(view)
		s.a.emitStatus(threadID)
		s.resumeIfReady(threadID)
	default:
		turn.Status = repo.TurnCompleted
		s.endTurn(turn)
	}
}

func (s *scheduler) resumeIfReady(threadID string) bool {
	if _, queued := s.slots[threadID]; queued {
		return false
	}
	view, err := s.a.ReadThread(threadID)
	if err != nil {
		return false
	}
	turn, active := view.ActiveTurn()
	if !active {
		return false
	}
	if _, waiting := view.PendingRequest(); waiting || len(view.AgentCalls()) > 0 {
		return false
	}
	s.enqueue(threadID, turn.ID)
	s.a.emitStatus(threadID)
	return true
}

func (s *scheduler) stop(threadID string, sl *slot) {
	if !s.queued(threadID) {
		sl.cancel()
		return
	}
	s.queue = slices.DeleteFunc(s.queue, func(w work) bool { return w.threadID == threadID })
	delete(s.slots, threadID)
	view, err := s.a.ReadThread(threadID)
	if err != nil {
		log.Printf("agent: thread %s cannot be read to stop it: %v", threadID, err)
		return
	}
	if turn, ok := view.ActiveTurn(); ok {
		turn.Status, turn.Error = repo.TurnInterrupted, "interrupted before it ran again"
		s.endTurn(turn)
	}
}

func (s *scheduler) keepTime() {
	defer s.workers.Done()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		s.mu.Lock()
		s.sendDue("")
		s.mu.Unlock()
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *scheduler) sendDue(threadID string) {
	if s.ctx.Err() != nil {
		return
	}
	now := time.Now().UTC()
	due, err := s.a.DB.DueCronJobs(now)
	if err != nil {
		log.Printf("agent: scheduled tasks: %v", err)
		return
	}
	for _, job := range due {
		if threadID != "" && job.ThreadID != threadID {
			continue
		}
		view, err := s.a.ReadThread(job.ThreadID)
		if repo.IsNotFound(err) {
			continue
		}
		if err != nil {
			log.Printf("agent: scheduled task %s: read thread %s: %v", job.ID, job.ThreadID, err)
			continue
		}
		if _, active := view.ActiveTurn(); active {
			continue
		}
		if err := s.sendJob(view, job); err != nil {
			log.Printf("agent: scheduled task %s could not start in thread %s: %v", job.ID, job.ThreadID, err)
		}
		if err := s.a.cron.Sent(job, now); err != nil {
			log.Printf("agent: scheduled task %s: record its run: %v", job.ID, err)
		}
	}
}

func (s *scheduler) sendJob(view repo.ThreadView, job repo.CronJobRow) error {
	text, err := checkMessage(job.Prompt)
	if err != nil {
		return err
	}
	model, err := s.a.conversationModel(view, "")
	if err != nil {
		return err
	}
	message := repo.NewUserMessage(uuid.NewString(), "", text, time.Now().UTC())
	message.CronJobID = job.ID
	_, err = s.beginTurn(view, message, model)
	return err
}

// recover rebuilds the queue from SQL: close items cut off by a restart, settle
// subagent calls whose children already ended, and enqueue every ready turn.
func (s *scheduler) recover() error {
	active, err := s.a.DB.ActiveThreadIDs()
	if err != nil {
		return fmt.Errorf("agent: active threads: %w", err)
	}
	for _, id := range active {
		closed, err := s.a.recoverItems(id)
		if err != nil {
			return fmt.Errorf("agent: recover thread %s: %w", id, err)
		}
		if len(closed) > 0 {
			log.Printf("agent: thread %s: closed %d items cut off by the restart", id, len(closed))
		}
	}
	for _, id := range active {
		view, err := s.a.ReadThread(id)
		if err != nil {
			log.Printf("agent: thread %s cannot be read during recovery: %v", id, err)
			continue
		}
		if turn, running := view.ActiveTurn(); running {
			if _, err := s.a.modelOn(turn.Model.Connection); err != nil {
				turn.Status, turn.Error = repo.TurnFailed, err.Error()
				s.endTurn(turn)
				for _, call := range view.AgentCalls() {
					s.stopThread(call.Await.ThreadID)
				}
				continue
			}
		}
		for _, call := range view.AgentCalls() {
			if settled, ok := s.settledCall(call); ok {
				if _, _, err := s.a.recordItem(id, settled); err != nil {
					return fmt.Errorf("agent: thread %s: settle Agent call %s: %w", id, call.ID, err)
				}
			}
		}
		s.launchSubagents(id)
	}
	for _, id := range active {
		s.resumeIfReady(id)
	}
	return nil
}

func (s *scheduler) settledCall(call repo.ItemRow) (repo.ItemRow, bool) {
	if call.Await.ThreadID == "" {
		return repo.ItemRow{}, false
	}
	child, err := s.a.ReadThread(call.Await.ThreadID)
	if repo.IsNotFound(err) {
		now := time.Now().UTC()
		call.Status, call.Error, call.CompletedAt = repo.ItemFailed, "the subagent's thread is missing", &now
		return call, true
	}
	if err != nil {
		return repo.ItemRow{}, false
	}
	if _, active := child.ActiveTurn(); active || len(child.Turns) == 0 {
		return repo.ItemRow{}, false
	}
	return report(call, child, child.Turns[len(child.Turns)-1]), true
}

func (a *Agent) StartTurn(threadID, text, connection string) (repo.TurnRow, error) {
	a.run.mu.Lock()
	defer a.run.mu.Unlock()
	return a.run.startTurn(threadID, text, connection)
}

func (a *Agent) SteerTurn(threadID, expectedTurnID, text string) (repo.TurnRow, error) {
	a.run.mu.Lock()
	defer a.run.mu.Unlock()
	return a.run.steer(threadID, expectedTurnID, text)
}

func (a *Agent) InterruptTurn(threadID string) error {
	a.run.mu.Lock()
	defer a.run.mu.Unlock()
	return a.run.interrupt(threadID)
}
