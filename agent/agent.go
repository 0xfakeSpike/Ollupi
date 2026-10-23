package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/memory"
	"github.com/0xfakeSpike/ollupi/agent/permission"
	"github.com/0xfakeSpike/ollupi/agent/skills"
	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/agent/tools/cron"
	"github.com/0xfakeSpike/ollupi/agent/tools/plan"
	"github.com/0xfakeSpike/ollupi/agent/tools/subagent"
	"github.com/0xfakeSpike/ollupi/agent/tools/userinput"
	"github.com/0xfakeSpike/ollupi/pkg/llm"
	"github.com/0xfakeSpike/ollupi/repo"
)

const (
	maxInputRunes = 4000
)

var (
	errTurnActive     = errors.New("the thread already has a turn in progress")
	ErrNoActiveTurn   = errors.New("the thread has no turn in progress")
	ErrTurnMismatch   = errors.New("the expected turn is not the turn in progress")
	errNoSuchRequest  = errors.New("the thread does not wait on this request")
	errSubagentThread = errors.New("a subagent thread takes no input; send it to its parent conversation")
)

type Agent struct {
	Name           string
	Prompt         string
	Models         []llm.Model
	Tools          []string
	ApprovalPolicy permission.Policy
	Permissions    permission.Rules

	Connections map[string]*llm.OpenAI
	Providers   []tools.Provider
	SkillsDir   string
	Memory      *memory.Manager
	Location    *time.Location
	DB          *repo.DB
	OnEvent     func(Event)

	ctx      context.Context
	registry *tools.Registry
	skills   *skills.Catalog
	cron     *cron.Jobs
	run      *scheduler
	loops    sync.WaitGroup
}

func (a *Agent) Start(ctx context.Context) error {
	if strings.TrimSpace(a.Name) == "" {
		return fmt.Errorf("agent: name is required")
	}
	if a.DB == nil {
		return fmt.Errorf("agent: a database is required")
	}
	a.ctx = ctx
	if err := a.buildTools(ctx); err != nil {
		return err
	}
	if err := a.checkModels(); err != nil {
		return err
	}
	if err := a.checkBudgets(); err != nil {
		return fmt.Errorf("agent %s: %w", a.Name, err)
	}
	a.run = newScheduler(a, ctx)
	if err := a.run.recover(); err != nil {
		return err
	}
	a.run.start()
	if a.Memory != nil {
		a.loops.Add(1)
		go func() {
			defer a.loops.Done()
			a.Memory.Run(ctx)
		}()
	}
	return nil
}

func (a *Agent) buildTools(ctx context.Context) error {
	location := a.Location
	if location == nil {
		location = time.Local
	}
	a.cron = cron.New(a.DB, location)
	providers := append(slices.Clone(a.Providers), plan.New(), subagent.New(), userinput.New(), a.cron)
	if a.SkillsDir != "" {
		var err error
		if a.skills, err = skills.New(a.SkillsDir); err != nil {
			return fmt.Errorf("agent: skills: %w", err)
		}
		providers = append(providers, a.skills)
	}
	var err error
	if a.registry, err = tools.New(ctx, providers...); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}

func (a *Agent) Wait() {
	a.run.wait()
	if a.ctx.Err() != nil {
		a.loops.Wait()
	}
}

func (a *Agent) CallTool(ctx context.Context, threadID, tool string, arguments map[string]any) (tools.Result, error) {
	caller := tools.Caller{}
	if threadID != "" {
		t, err := a.DB.ThreadRow(threadID)
		if err != nil {
			return tools.Result{}, fmt.Errorf("agent: thread %s: %w", threadID, err)
		}
		caller = a.callerOf(t)
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	return a.registry.Call(ctx, caller, tool, arguments), nil
}

func (a *Agent) callerOf(t repo.ThreadRow) tools.Caller {
	c := tools.Caller{ThreadID: t.ID, WorkspaceID: t.WorkspaceID, Subagent: t.ParentThreadID != ""}
	if w, err := a.DB.Workspace(t.WorkspaceID); err == nil {
		c.Root = w.RootPath
	}
	return c
}

// policyFor resolves the approval policy of a thread: thread override, then
// workspace setting, then the agent default.
