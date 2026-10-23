package runtime

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/contextmanager"
	"github.com/0xfakeSpike/ollupi/agent/memory"
	"github.com/0xfakeSpike/ollupi/agent/permission"
	"github.com/0xfakeSpike/ollupi/agent/skills"
	"github.com/0xfakeSpike/ollupi/agent/systemprompt"
	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/pkg/llm"
	"github.com/0xfakeSpike/ollupi/pkg/redact"
	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

const (
	maxModelCalls          = 200
	maxToolCalls           = 400
	maxConsecutiveFailures = 10
)

type Runtime struct {
	LLM      *llm.OpenAI
	Registry *tools.Registry
	Skills   *skills.Catalog
	Contexts *contextmanager.Manager
	Memory   *memory.Manager

	Turn        repo.TurnRow
	Caller      tools.Caller
	Prompt      string
	Tools       []tools.ToolDescriptor
	Permissions *permission.Checker
	History     []repo.ItemRow
	Items       []repo.ItemRow
	Record      func(repo.ItemRow) (repo.ItemRow, error)
	Delta       func(repo.ItemRow, string)

	system   string
	redactor *redact.Redactor
	usage    repo.Usage
}

type Outcome int

const (
	Continue Outcome = iota
	Wait
	Done
)

func (rt *Runtime) Step(ctx context.Context) (Outcome, repo.Usage, error) {
	ctx = tools.WithCaller(ctx, rt.Caller)
	if err := rt.start(); err != nil {
		return Done, repo.Usage{}, err
	}
	outcome, err := rt.run(ctx)
	return outcome, rt.usage, err
}

func (rt *Runtime) start() error {
	for _, it := range rt.Items {
		if it.IsPending() || it.Awaits() {
			return fmt.Errorf("runtime: item %s still waits for its answer", it.ID)
		}
	}
	rt.redactor = redact.New()
	var catalog []skills.Skill
	if rt.Skills != nil {
		catalog = rt.Skills.All()
	}
	rt.system = systemprompt.Build(systemprompt.Input{
		Agent: rt.Prompt, Subagent: rt.Caller.Subagent, Root: rt.Caller.Root,
		Skills: catalog, SkillTokens: rt.Contexts.SkillBudget(),
	})
	if rt.Memory != nil {
		if it, ok := rt.Memory.TurnContext(rt.Turn.ID, rt.Caller.WorkspaceID, rt.Items, rt.Contexts.MemoryBudget()); ok {
			if _, err := rt.record(it); err != nil {
				return err
			}
		}
	}
	return nil
}

func (rt *Runtime) tool(name string) (tools.ToolDescriptor, bool) {
	i := slices.IndexFunc(rt.Tools, func(d tools.ToolDescriptor) bool { return d.Name == name })
	if i < 0 {
		return tools.ToolDescriptor{}, false
	}
	return rt.Tools[i], true
}

func (rt *Runtime) specs() []llm.ToolSpec { return Specs(rt.Tools) }

// Specs are the tool definitions a request carries for these tools.
func Specs(descriptors []tools.ToolDescriptor) []llm.ToolSpec {
	specs := make([]llm.ToolSpec, 0, len(descriptors))
	for _, d := range descriptors {
		specs = append(specs, llm.ToolSpec{Name: d.Name, Description: strings.TrimSpace(d.Description), Parameters: d.InputSchema})
	}
	return specs
}

func (rt *Runtime) run(ctx context.Context) (Outcome, error) {
	if outcome, ended, err := rt.act(ctx); ended {
		return outcome, err
	}
	if rt.modelCalls() >= maxModelCalls {
		return Done, rt.finish(ctx, fmt.Sprintf("reached the model call limit (%d)", maxModelCalls))
	}
	resp, err := rt.respond(ctx, llm.ToolChoiceAuto)
	if err != nil {
		return Done, err
	}
	if len(resp.Message.ToolCalls) == 0 {
		return Done, nil
	}
	if outcome, ended, err := rt.act(ctx); ended {
		return outcome, err
	}
	return Continue, nil
}

func (rt *Runtime) act(ctx context.Context) (Outcome, bool, error) {
	halt, pending, err := rt.handleCalls(ctx)
	if err != nil {
		return Done, true, err
	}
	if pending != nil {
		return Wait, true, nil
	}
	if rt.awaiting() {
		return Wait, true, nil
	}
	if halt != "" {
		return Done, true, rt.finish(ctx, halt)
	}
	return Continue, false, nil
}

const finalAnswerNotice = `[runtime] Tool use has stopped: %s.
Do not call tools. Write the final answer to the user now, in their language: report what was accomplished, citing the observations above; state clearly what could not be done and why; and give the most useful next step.`

func (rt *Runtime) finish(ctx context.Context, reason string) error {
	now := time.Now().UTC()
	if _, err := rt.record(repo.ItemRow{
		ID: uuid.NewString(), TurnID: rt.Turn.ID, Type: repo.ItemError, Status: repo.ItemCompleted,
		Text: fmt.Sprintf(finalAnswerNotice, reason), StartedAt: now, CompletedAt: &now,
	}); err != nil {
		return err
	}
	if _, err := rt.respond(ctx, llm.ToolChoiceNone); err != nil {
		return fmt.Errorf("%w (tool use stopped: %s)", err, reason)
	}
	return nil
}

func (rt *Runtime) record(it repo.ItemRow) (repo.ItemRow, error) {
	stored, err := rt.Record(it)
	if err != nil {
		return repo.ItemRow{}, fmt.Errorf("runtime: record %s item: %w", it.Type, err)
	}
	for i := range rt.Items {
		if rt.Items[i].ID == stored.ID {
			rt.Items[i] = stored
			return stored, nil
		}
	}
	rt.Items = append(rt.Items, stored)
	return stored, nil
}

func (rt *Runtime) complete(it repo.ItemRow, status repo.ItemStatus) (repo.ItemRow, error) {
	now := time.Now().UTC()
	it.Status = status
	it.CompletedAt = &now
	return rt.record(it)
}

func withError(it repo.ItemRow, text string) repo.ItemRow {
	it.Error = text
	return it
}

func (rt *Runtime) awaiting() bool {
	return slices.ContainsFunc(rt.Items, repo.ItemRow.Awaits)
}
