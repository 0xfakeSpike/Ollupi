// Package agent defines an Agent (name, instructions, model, tools, approval
// policy, as in the OpenAI Agents SDK and Claude's AgentDefinition) and the
// Runner that executes one turn of it: a serial ReAct loop with one model and
// one tool call at a time. The model decides when the work is done by
// answering without a tool call.
//
// The Runner never keeps hidden state: every fact (model text, reasoning,
// tool call, approval, result, plan, question) is recorded as a thread item
// before the Runner acts on it, and everything it counts or shows the model
// is derived from those items. When a call needs the user's approval or
// answers, Run records the request and returns; running the turn again once
// the item holds the response continues exactly where it stopped.
package agent

import (
	"fmt"
	"strings"
)

// Agent is the definition of one agent.
type Agent struct {
	Name string
	// Instructions are the agent's own system instructions. The Runner adds
	// its loop rules, the workspace AGENTS.md, and the skills catalog.
	Instructions string
	// Model is the model name sent with every request.
	Model          string
	ModelProfileID string
	ModelSettings  ModelSettings
	// InheritModel marks a subagent whose model connection follows the
	// parent turn. Explicit subagent models keep their configured connection.
	InheritModel bool
	// Tools lists the registry tools the agent may call, by exact name. Nil
	// means every registry tool; empty means none. update_plan and request_user_input
	// are always offered.
	Tools          []string
	ApprovalPolicy ApprovalPolicy
	// Permissions are allow, ask, and deny rules, checked before the
	// approval policy.
	Permissions Permissions
	// Subagents are the agents this agent may launch with the Agent tool.
	// None means no Agent tool; subagents themselves have none.
	Subagents []Subagent
}

// ModelSettings are optional per-request model parameters.
type ModelSettings struct {
	// EnableThinking, when set, is sent as the provider's "enable_thinking"
	// extension (Qwen/DashScope).
	EnableThinking *bool
}

// DefaultInstructions are used when the configuration gives none.
const DefaultInstructions = "You are Ollupi, a personal assistant agent."

// Validate checks the definition itself. Tool names are checked against the
// catalog by Runner.Check.
func (a Agent) Validate() error {
	if strings.TrimSpace(a.Name) == "" {
		return fmt.Errorf("agent: name is required")
	}
	if strings.TrimSpace(a.Instructions) == "" {
		return fmt.Errorf("agent %s: instructions are required", a.Name)
	}
	if strings.TrimSpace(a.Model) == "" {
		return fmt.Errorf("agent %s: model is required", a.Name)
	}
	if _, err := ParseApprovalPolicy(string(a.ApprovalPolicy)); err != nil {
		return fmt.Errorf("agent %s: %w", a.Name, err)
	}
	if _, err := a.Permissions.parse(); err != nil {
		return fmt.Errorf("agent %s: %w", a.Name, err)
	}
	launchable := make(map[string]bool, len(a.Subagents))
	for _, s := range a.Subagents {
		if launchable[s.Name] {
			return fmt.Errorf("agent %s: subagent %q is listed twice", a.Name, s.Name)
		}
		if strings.TrimSpace(s.Description) == "" {
			return fmt.Errorf("agent %s: subagent %q has no description", a.Name, s.Name)
		}
		launchable[s.Name] = true
	}
	seen := make(map[string]bool, len(a.Tools))
	for _, name := range a.Tools {
		if seen[name] {
			return fmt.Errorf("agent %s: tool %q is listed twice", a.Name, name)
		}
		seen[name] = true
	}
	return nil
}
