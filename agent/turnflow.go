package agent

import (
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/agent/tools/subagent"
	"github.com/0xfakeSpike/ollupi/agent/tools/userinput"
	"github.com/0xfakeSpike/ollupi/pkg/llm"
	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

func (s *scheduler) startTurn(threadID, text, connection string) (repo.TurnRow, error) {
	text, err := checkMessage(text)
	if err != nil {
		return repo.TurnRow{}, err
	}
	view, err := s.a.ReadThread(threadID)
	if err != nil {
		return repo.TurnRow{}, err
	}
	if view.Thread.ParentThreadID != "" {
		return repo.TurnRow{}, errSubagentThread
	}
	model, err := s.a.conversationModel(view, connection)
	if err != nil {
		return repo.TurnRow{}, err
	}
	return s.beginTurn(view, repo.NewUserMessage(uuid.NewString(), "", text, time.Now().UTC()), model)
}

func (s *scheduler) beginTurn(view repo.ThreadView, message repo.ItemRow, model llm.Model) (repo.TurnRow, error) {
	threadID := view.Thread.ID
	if _, active := view.ActiveTurn(); active {
		return repo.TurnRow{}, errTurnActive
	}
	if view.Thread.ParentThreadID == "" {
		if err := s.a.checkWorkspace(view.Thread.WorkspaceID); err != nil {
			return repo.TurnRow{}, err
		}
	}
	policy := s.a.policyFor(view.Thread)
	turn := repo.TurnRow{
		ID: uuid.NewString(), ThreadID: threadID, Status: repo.TurnInProgress,
		Model: model.Clone(), ApprovalPolicy: string(policy), Permissions: s.a.rulesFor(view.Thread).ToRepo(), StartedAt: time.Now().UTC(),
	}
	message.TurnID = turn.ID
	turn, err := s.a.insertTurn(threadID, turn, []repo.ItemRow{message})
	if err != nil {
		return repo.TurnRow{}, err
	}
	s.a.emit(Event{Kind: EventTurnStarted, Thread: view.Thread, Turn: turn})
	s.a.emitItem(view.Thread, turn, message, true)
	s.enqueue(threadID, turn.ID)
	s.a.emitStatus(threadID)
	return turn, nil
}

func (s *scheduler) steer(threadID, expectedTurnID, text string) (repo.TurnRow, error) {
	text, err := checkMessage(text)
	if err != nil {
		return repo.TurnRow{}, err
	}
	view, err := s.a.ReadThread(threadID)
	if err != nil {
		return repo.TurnRow{}, err
	}
	if view.Thread.ParentThreadID != "" {
		return repo.TurnRow{}, errSubagentThread
	}
	turn, active := view.ActiveTurn()
	if !active {
		return repo.TurnRow{}, ErrNoActiveTurn
	}
	if turn.ID != expectedTurnID {
		return repo.TurnRow{}, fmt.Errorf("%w (expected %s, in progress %s)", ErrTurnMismatch, expectedTurnID, turn.ID)
	}
	stored, isNew, err := s.a.recordItem(threadID, repo.NewUserMessage(uuid.NewString(), turn.ID, text, time.Now().UTC()))
	if err != nil {
		return repo.TurnRow{}, err
	}
	s.a.emitItem(view.Thread, turn, stored, isNew)
	if sl, ok := s.slots[threadID]; ok && !s.queued(threadID) {
		sl.steered = true
	}
	return turn, nil
}

func (s *scheduler) interrupt(threadID string) error {
	if sl, ok := s.slots[threadID]; ok {
		s.stop(threadID, sl)
		return nil
	}
	view, err := s.a.ReadThread(threadID)
	if err != nil {
		return err
	}
	turn, active := view.ActiveTurn()
	if !active {
		return ErrNoActiveTurn
	}
	calls := view.AgentCalls()
	turn.Status = repo.TurnInterrupted
	turn.Error = "interrupted while waiting for the user"
	if len(calls) > 0 {
		turn.Error = "interrupted while waiting for subagents"
	}
	s.endTurn(turn)
	for _, call := range calls {
		s.stopThread(call.Await.ThreadID)
	}
	return nil
}

func (s *scheduler) stopThread(threadID string) {
	if sl, ok := s.slots[threadID]; ok {
		s.stop(threadID, sl)
		return
	}
	view, err := s.a.ReadThread(threadID)
	if err != nil {
		return
	}
	if turn, active := view.ActiveTurn(); active {
		turn.Status = repo.TurnInterrupted
		turn.Error = "stopped with the turn that launched it"
		s.endTurn(turn)
	}
}

func (s *scheduler) endTurn(turn repo.TurnRow) {
	view, err := s.a.ReadThread(turn.ThreadID)
	if err != nil {
		log.Printf("agent: thread %s cannot be read to end turn %s: %v", turn.ThreadID, turn.ID, err)
		return
	}
	var closed []repo.ItemRow
	report, parent, reports := s.reportLocked(view, turn)
	if reports {
		var stored repo.ItemRow
		closed, stored, err = s.a.completeSubagentTurn(turn.ThreadID, turn, report)
		if err == nil {
			s.a.emitItem(parent.Thread, parent.Turn, stored, false)
		}
	} else {
		closed, err = s.a.completeTurn(turn.ThreadID, turn)
	}
	if err != nil {
		log.Printf("agent: thread %s: record end of turn %s: %v", turn.ThreadID, turn.ID, err)
	}
	view, err = s.a.ReadThread(turn.ThreadID)
	if err != nil {
		log.Printf("agent: thread %s cannot be read after turn %s ended: %v", turn.ThreadID, turn.ID, err)
		return
	}
	for _, it := range closed {
		s.a.emitItem(view.Thread, turn, it, false)
	}
	ended, _ := findTurn(view, turn.ID)
	s.a.emit(Event{Kind: EventTurnCompleted, Thread: view.Thread, Turn: ended})
	s.a.emitStatus(turn.ThreadID)
	if reports && err == nil {
		s.resumeIfReady(parent.Thread.ID)
	}
	s.sendDue(turn.ThreadID)
}

type parentTurn struct {
	Thread repo.ThreadRow
	Turn   repo.TurnRow
}

func (s *scheduler) launchSubagents(threadID string) {
	view, err := s.a.ReadThread(threadID)
	if err != nil {
		log.Printf("agent: thread %s cannot be read to launch subagents: %v", threadID, err)
		return
	}
	turn, _ := view.ActiveTurn()
	for _, call := range view.AgentCalls() {
		if call.Await.ThreadID != "" {
			continue
		}
		if child, err := s.launchSubagent(view.Thread, turn, call); err != nil {
			now := time.Now().UTC()
			call.Status, call.Error, call.CompletedAt = repo.ItemFailed, fmt.Sprintf("the subagent could not be launched: %v", err), &now
		} else {
			call.Await.ThreadID = child.ID
		}
		stored, _, err := s.a.recordItem(threadID, call)
		if err != nil {
			log.Printf("agent: thread %s: record Agent call %s: %v", threadID, call.ID, err)
			continue
		}
		s.a.emitItem(view.Thread, turn, stored, false)
	}
}

func (s *scheduler) launchSubagent(parent repo.ThreadRow, parentTurn repo.TurnRow, call repo.ItemRow) (repo.ThreadRow, error) {
	if parent.ParentThreadID != "" {
		return repo.ThreadRow{}, fmt.Errorf("a subagent cannot launch subagents; do the task yourself with your tools")
	}
	req, err := subagent.Decode(call.Await.Request)
	if err != nil {
		return repo.ThreadRow{}, err
	}
	child, err := s.a.createThread(repo.ThreadRow{
		ParentThreadID: parent.ID, ParentItemID: call.ID,
		ApprovalPolicy: parent.ApprovalPolicy, WorkspaceID: parent.WorkspaceID,
	})
	if err != nil {
		return repo.ThreadRow{}, err
	}
	s.a.emit(Event{Kind: EventThreadStarted, Thread: child})
	view, err := s.a.ReadThread(child.ID)
	if err != nil {
		return repo.ThreadRow{}, err
	}
	if _, err := s.beginTurn(view, repo.NewUserMessage(uuid.NewString(), "", req.Prompt, time.Now().UTC()), parentTurn.Model); err != nil {
		return repo.ThreadRow{}, err
	}
	return child, nil
}

func (s *scheduler) reportLocked(child repo.ThreadView, ended repo.TurnRow) (repo.ItemRow, parentTurn, bool) {
	if child.Thread.ParentThreadID == "" {
		return repo.ItemRow{}, parentTurn{}, false
	}
	parent, err := s.a.ReadThread(child.Thread.ParentThreadID)
	if err != nil {
		return repo.ItemRow{}, parentTurn{}, false
	}
	turn, active := parent.ActiveTurn()
	if !active {
		return repo.ItemRow{}, parentTurn{}, false
	}
	for _, call := range parent.AgentCalls() {
		if call.ID == child.Thread.ParentItemID {
			return report(call, child, ended), parentTurn{Thread: parent.Thread, Turn: turn}, true
		}
	}
	return repo.ItemRow{}, parentTurn{}, false
}

func report(call repo.ItemRow, child repo.ThreadView, ended repo.TurnRow) repo.ItemRow {
	now := time.Now().UTC()
	call.CompletedAt = &now
	switch ended.Status {
	case repo.TurnCompleted:
		if answer, ok := child.FinalAnswer(ended.ID); ok {
			result, _, err := repo.ResultOf(tools.Result{Output: answer})
			if err == nil {
				call.Status, call.Result = repo.ItemCompleted, result
				return call
			}
		}
		call.Status, call.Error = repo.ItemFailed, "the subagent finished without a report"
	case repo.TurnInterrupted:
		call.Status, call.Error = repo.ItemFailed, "the subagent was stopped: "+ended.Error
	default:
		call.Status, call.Error = repo.ItemFailed, "the subagent failed: "+ended.Error
	}
	return call
}

func (s *scheduler) resolveApproval(threadID, approvalID string, decision repo.Decision, decidedBy string) error {
	return s.respond(threadID, func(it repo.ItemRow) bool { return it.WaitsForApproval() && it.Approval.ID == approvalID }, func(it repo.ItemRow) (repo.ItemRow, error) {
		if available := it.Approval.AvailableDecisions(); !slices.Contains(available, decision) {
			return repo.ItemRow{}, fmt.Errorf("agent: decision %q is not one of %q", decision, available)
		}
		now := time.Now().UTC()
		it.Approval.Decision, it.Approval.DecidedBy, it.Approval.DecidedAt = decision, decidedBy, &now
		return it, nil
	})
}

func (s *scheduler) answerUserInput(threadID, itemID string, answers map[string]userinput.Answer) error {
	return s.respond(threadID, func(it repo.ItemRow) bool { return it.ID == itemID && !it.WaitsForApproval() }, func(it repo.ItemRow) (repo.ItemRow, error) {
		req, err := userinput.Decode(it.Await.Request)
		if err != nil {
			return repo.ItemRow{}, err
		}
		if err := req.Check(answers, maxInputRunes); err != nil {
			return repo.ItemRow{}, fmt.Errorf("agent: %w", err)
		}
		result, _, err := repo.ResultOf(tools.Result{Output: answers})
		if err != nil {
			return repo.ItemRow{}, err
		}
		now := time.Now().UTC()
		it.Result, it.Status, it.CompletedAt = result, repo.ItemCompleted, &now
		return it, nil
	})
}

func (s *scheduler) respond(threadID string, match func(repo.ItemRow) bool, apply func(repo.ItemRow) (repo.ItemRow, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	view, err := s.a.ReadThread(threadID)
	if err != nil {
		return err
	}
	if _, scheduled := s.slots[threadID]; scheduled {
		return errNoSuchRequest
	}
	i := slices.IndexFunc(view.PendingRequests(), match)
	if i < 0 {
		return errNoSuchRequest
	}
	pending := view.PendingRequests()[i]
	activeTurn, _ := view.ActiveTurn()
	answered, err := apply(pending)
	if err != nil {
		return err
	}
	stored, _, err := s.a.recordItem(threadID, answered)
	if err != nil {
		return err
	}
	s.a.emitItem(view.Thread, activeTurn, stored, false)
	if !s.resumeIfReady(threadID) {
		s.a.emitStatus(threadID)
	}
	return nil
}
