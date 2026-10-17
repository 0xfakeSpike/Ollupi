package threadmanager

import (
	"fmt"
	"strings"
	"sync"

	"github.com/0xfakeSpike/ollupi/thread"
)

// SteerTurn sends a user message to the thread's running or queued turn
// (Codex's turn/steer). The Runner takes it before its next model call, so
// the turn goes on with it; no new turn starts. expectedTurnID must be the
// turn in progress. A turn waiting for the user, or one that is ending,
// takes no message: ErrTurnNotSteerable.
func (m *Manager) SteerTurn(threadID, expectedTurnID, text string) (thread.Turn, error) {
	text, err := checkMessage(text)
	if err != nil {
		return thread.Turn{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	view, ok := m.threads.Read(threadID)
	if !ok {
		return thread.Turn{}, fmt.Errorf("threadmanager: thread %s not found", threadID)
	}
	if view.Thread.Source == thread.SourceSubagent {
		return thread.Turn{}, ErrSubagentThread
	}
	turn, active := view.ActiveTurn()
	if !active {
		return thread.Turn{}, ErrNoActiveTurn
	}
	if turn.ID != expectedTurnID {
		return thread.Turn{}, fmt.Errorf("%w (expected %s, in progress %s)", ErrTurnMismatch, expectedTurnID, turn.ID)
	}
	ex, scheduled := m.executions[threadID]
	if !scheduled || !ex.input.push(text) {
		return thread.Turn{}, ErrTurnNotSteerable
	}
	return turn, nil
}

// checkMessage trims a user message and enforces its bounds.
func checkMessage(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("threadmanager: empty message")
	}
	if n := len([]rune(text)); n > MaxInputRunes {
		return "", fmt.Errorf("threadmanager: message has %d characters; at most %d are accepted", n, MaxInputRunes)
	}
	return text, nil
}

// pendingInput holds the messages sent to one run of a turn until the
// Runner takes them. It implements agent.PendingInput. Once closed it
// refuses messages; whatever the Runner did not take is returned by close.
type pendingInput struct {
	mu     sync.Mutex
	texts  []string
	closed bool
}

func (p *pendingInput) push(text string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.texts = append(p.texts, text)
	return true
}

func (p *pendingInput) Take() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	texts := p.texts
	p.texts = nil
	return texts
}

func (p *pendingInput) TakeOrClose() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.texts) == 0 {
		p.closed = true
	}
	texts := p.texts
	p.texts = nil
	return texts
}

// close refuses further messages and returns those not taken.
func (p *pendingInput) close() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	texts := p.texts
	p.texts = nil
	return texts
}
