package threadmanager

import (
	"fmt"
	"strings"

	"github.com/0xfakeSpike/ollupi/agent"
	"github.com/0xfakeSpike/ollupi/thread"
)

// ModelProfile contains runtime metadata only. Credentials belong solely to
// the model clients in the composition root, never the thread manager.
type ModelProfile struct {
	ID       string              `json:"id"`
	Name     string              `json:"name"`
	Model    string              `json:"model"`
	Settings agent.ModelSettings `json:"-"`
}

type ModelList struct {
	Data                  []ModelProfile `json:"data"`
	DefaultModelProfileID string         `json:"defaultModelProfileId"`
}

func (m *Manager) configureModels(profiles []ModelProfile) error {
	if m.agent.ModelProfileID == "" {
		m.agent.ModelProfileID = "primary"
	}
	if len(profiles) == 0 {
		profiles = []ModelProfile{{ID: "primary", Name: "Primary connection", Model: m.agent.Model, Settings: m.agent.ModelSettings}}
	}
	m.profiles = make(map[string]ModelProfile, len(profiles))
	for _, p := range profiles {
		if p.ID == "" || strings.TrimSpace(p.Model) == "" {
			return fmt.Errorf("threadmanager: model profile needs an ID and model")
		}
		if _, exists := m.profiles[p.ID]; exists {
			return fmt.Errorf("threadmanager: duplicate model profile %q", p.ID)
		}
		p.Settings.EnableThinking = copyBool(p.Settings.EnableThinking)
		m.profiles[p.ID] = p
		m.modelOrder = append(m.modelOrder, p.ID)
	}
	if _, err := m.modelProfile(m.agent.ModelProfileID); err != nil {
		return err
	}
	return nil
}

func (m *Manager) ListModels() ModelList {
	data := make([]ModelProfile, 0, len(m.modelOrder))
	for _, id := range m.modelOrder {
		p := m.profiles[id]
		p.Settings = agent.ModelSettings{}
		data = append(data, p)
	}
	return ModelList{Data: data, DefaultModelProfileID: m.agent.ModelProfileID}
}

func (m *Manager) modelProfile(id string) (ModelProfile, error) {
	p, ok := m.profiles[id]
	if !ok {
		return ModelProfile{}, fmt.Errorf("model profile %q is unavailable; restore it in Settings or select another model for a new turn", id)
	}
	return p, nil
}

// agentForNewTurn resolves the selection exactly once, before persisting the
// turn. Existing turns always use agentForTurn instead.
func (m *Manager) agentForNewTurn(view thread.View, requested string) (agent.Agent, error) {
	a, err := m.agentFor(view.Thread)
	if err != nil {
		return a, err
	}
	if view.Thread.Source == thread.SourceSubagent {
		if a.InheritModel {
			parent, ok := m.threads.Read(view.Thread.ParentThreadID)
			if !ok {
				return a, fmt.Errorf("subagent parent is unavailable")
			}
			turn, active := parent.ActiveTurn()
			if !active {
				return a, fmt.Errorf("subagent parent has no active turn")
			}
			return m.withTurnModel(a, turn)
		}
		if a.ModelProfileID == "" {
			a.ModelProfileID = m.agent.ModelProfileID
		}
		_, err := m.modelProfile(a.ModelProfileID)
		return a, err
	}
	id := strings.TrimSpace(requested)
	if id == "" && len(view.Turns) > 0 {
		id = view.Turns[len(view.Turns)-1].ModelProfileID
		if id == "" {
			id = "primary"
		}
	}
	if id == "" {
		id = m.agent.ModelProfileID
	}
	p, err := m.modelProfile(id)
	if err != nil {
		return a, err
	}
	a.ModelProfileID, a.Model = p.ID, p.Model
	a.ModelSettings = agent.ModelSettings{EnableThinking: copyBool(p.Settings.EnableThinking)}
	return a, nil
}

func (m *Manager) agentForTurn(t thread.Thread, turn thread.Turn) (agent.Agent, error) {
	a, err := m.agentFor(t)
	if err != nil {
		return a, err
	}
	return m.withTurnModel(a, turn)
}

func (m *Manager) withTurnModel(a agent.Agent, turn thread.Turn) (agent.Agent, error) {
	id := turn.ModelProfileID
	if id == "" {
		id = "primary"
	}
	if _, err := m.modelProfile(id); err != nil {
		return a, err
	}
	a.ModelProfileID, a.Model = id, turn.Model
	a.ModelSettings = agent.ModelSettings{EnableThinking: copyBool(turn.ModelSettings.EnableThinking)}
	return a, nil
}

func copyBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
