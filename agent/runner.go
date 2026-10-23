package agent

import (
	"context"

	"github.com/0xfakeSpike/ollupi/agent/permission"
	"github.com/0xfakeSpike/ollupi/agent/runtime"
	"github.com/0xfakeSpike/ollupi/agent/systemprompt"
	"github.com/0xfakeSpike/ollupi/repo"
)

// step assembles one iteration of a turn: the runtime executor filled with the
// turn's immutable context and this iteration's items.
func (a *Agent) step(ctx context.Context, view repo.ThreadView, turn repo.TurnRow) (runtime.Outcome, repo.Usage, error) {
	if _, err := a.modelOn(turn.Model.Connection); err != nil {
		return runtime.Done, repo.Usage{}, err
	}
	contexts, err := a.contextsFor(turn.Model)
	if err != nil {
		return runtime.Done, repo.Usage{}, err
	}
	descriptors, checker, err := a.toolsFor(a.Tools, permission.Policy(turn.ApprovalPolicy), permission.RulesFrom(turn.Permissions))
	if err != nil {
		return runtime.Done, repo.Usage{}, err
	}
	var history, items []repo.ItemRow
	for _, it := range view.Items {
		if it.TurnID == turn.ID {
			items = append(items, it)
		} else {
			history = append(history, it)
		}
	}
	rt := &runtime.Runtime{
		LLM:         a.Connections[turn.Model.Connection],
		Registry:    a.registry,
		Skills:      a.skills,
		Contexts:    contexts,
		Memory:      a.Memory,
		Turn:        turn,
		Caller:      a.callerOf(view.Thread),
		Prompt:      systemprompt.AgentPrompt(a.Name, a.Prompt),
		Tools:       descriptors,
		Permissions: checker,
		History:     history,
		Items:       items,
		Record:      a.recorder(view.Thread, turn),
		Delta: func(it repo.ItemRow, text string) {
			a.emit(Event{Kind: EventItemDelta, Thread: view.Thread, Turn: turn, Item: &it, Delta: text})
		},
	}
	return rt.Step(ctx)
}

func (a *Agent) recorder(t repo.ThreadRow, turn repo.TurnRow) func(repo.ItemRow) (repo.ItemRow, error) {
	return func(it repo.ItemRow) (repo.ItemRow, error) {
		stored, isNew, err := a.recordItem(t.ID, it)
		if err != nil {
			return repo.ItemRow{}, err
		}
		a.emitItem(t, turn, stored, isNew)
		return stored, nil
	}
}

func findTurn(view repo.ThreadView, turnID string) (repo.TurnRow, bool) {
	for _, t := range view.Turns {
		if t.ID == turnID {
			return t, true
		}
	}
	return repo.TurnRow{}, false
}
