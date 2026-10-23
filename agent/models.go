package agent

import (
	"fmt"
	"slices"
	"strings"

	"github.com/0xfakeSpike/ollupi/agent/contextmanager"
	"github.com/0xfakeSpike/ollupi/agent/runtime"
	"github.com/0xfakeSpike/ollupi/agent/skills"
	"github.com/0xfakeSpike/ollupi/agent/systemprompt"
	"github.com/0xfakeSpike/ollupi/pkg/llm"
	"github.com/0xfakeSpike/ollupi/repo"
)

type ModelList struct {
	Data    []llm.Model `json:"data"`
	Default string      `json:"default"`
}

func (a *Agent) checkModels() error {
	if len(a.Models) == 0 {
		return fmt.Errorf("agent %s: at least one model is required", a.Name)
	}
	for i, m := range a.Models {
		if m.Connection == "" || strings.TrimSpace(m.Name) == "" {
			return fmt.Errorf("agent: every model needs a connection and a name")
		}
		if slices.ContainsFunc(a.Models[:i], func(o llm.Model) bool { return o.Connection == m.Connection }) {
			return fmt.Errorf("agent: connection %q is listed twice", m.Connection)
		}
		if a.Connections[m.Connection] == nil {
			return fmt.Errorf("agent: connection %q has no client", m.Connection)
		}
	}
	return nil
}

// contextsFor builds the prompts of a turn on model m, within the limits its
// connection reports for it.
func (a *Agent) contextsFor(m llm.Model) (*contextmanager.Manager, error) {
	conn := a.Connections[m.Connection]
	if conn == nil {
		return nil, fmt.Errorf("connection %q has no client", m.Connection)
	}
	limits, err := conn.Limits(m.Name)
	if err != nil {
		return nil, err
	}
	return contextmanager.New(m.Name, limits), nil
}

// checkBudgets makes sure every model has known limits and room for a
// conversation next to the system prompt and the tool definitions.
func (a *Agent) checkBudgets() error {
	descriptors, _, err := a.toolsFor(a.Tools, a.ApprovalPolicy, a.Permissions)
	if err != nil {
		return err
	}
	var catalog []skills.Skill
	if a.skills != nil {
		catalog = a.skills.All()
	}
	for _, m := range a.Models {
		contexts, err := a.contextsFor(m)
		if err != nil {
			return err
		}
		system := systemprompt.Build(systemprompt.Input{Agent: systemprompt.AgentPrompt(a.Name, a.Prompt), Skills: catalog, SkillTokens: contexts.SkillBudget()})
		if err := contexts.Fits(system, runtime.Specs(descriptors)); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) ListModels() ModelList {
	data := make([]llm.Model, 0, len(a.Models))
	for _, m := range a.Models {
		data = append(data, m.Clone())
	}
	return ModelList{Data: data, Default: a.Models[0].Connection}
}

func (a *Agent) modelOn(connection string) (llm.Model, error) {
	i := slices.IndexFunc(a.Models, func(m llm.Model) bool { return m.Connection == connection })
	if i < 0 {
		return llm.Model{}, fmt.Errorf("connection %q is unavailable; restore it in Settings or select another model for a new turn", connection)
	}
	return a.Models[i].Clone(), nil
}

func (a *Agent) conversationModel(view repo.ThreadView, requested string) (llm.Model, error) {
	connection := strings.TrimSpace(requested)
	if connection == "" && len(view.Turns) > 0 {
		connection = view.Turns[len(view.Turns)-1].Model.Connection
	}
	if connection == "" {
		connection = a.Models[0].Connection
	}
	return a.modelOn(connection)
}
