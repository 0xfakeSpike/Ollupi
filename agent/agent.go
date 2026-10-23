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
	workers       = 4
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

	ctx        context.Context
	registry   *tools.Registry
	skills     *skills.Catalog
	cron       *cron.Jobs
	recovering bool
	slots      map[string]*slot
	queue      []string
	busy       int
	mu         sync.Mutex
	background sync.WaitGroup
	loops      sync.WaitGroup
}

type slot struct {
	cancel  context.CancelFunc
	steered bool
}

func (a *Agent) Start(ctx context.Context) error {
	if strings.TrimSpace(a.Name) == "" {
		return fmt.Errorf("agent: name is required")
	}
	if a.DB == nil {
		return fmt.Errorf("agent: a database is required")
	}
	a.ctx = ctx
	a.slots = map[string]*slot{}
	if err := a.buildTools(ctx); err != nil {
		return err
	}
	if err := a.checkModels(); err != nil {
		return err
	}
	if err := a.checkBudgets(); err != nil {
		return fmt.Errorf("agent %s: %w", a.Name, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recovering = true
	if err := a.recoverLocked(); err != nil {
		return err
	}
	a.recovering = false
	a.dispatchLocked()
	a.loops.Add(1)
	go a.keepTime()
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
	a.background.Wait()
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

func (a *Agent) threadPolicy(t repo.ThreadRow) permission.Policy {
	if t.ApprovalPolicy != "" {
		return permission.Policy(t.ApprovalPolicy)
	}
	return a.ApprovalPolicy
}

func (a *Agent) rulesFor(t repo.ThreadRow) permission.Rules {
	if t.ParentThreadID == "" {
		return a.Permissions
	}
	rules := a.Permissions
	rules.Deny = append(slices.Clone(rules.Deny), subagent.Tool)
	return rules
}

func (a *Agent) toolsFor(names []string, policy permission.Policy, rules permission.Rules) ([]tools.ToolDescriptor, *permission.Checker, error) {
	checker, err := permission.New(policy, rules, a.registry)
	if err != nil {
		return nil, nil, err
	}
	catalog := a.registry.Descriptors()
	descriptors := catalog
	if names != nil {
		for i, name := range names {
			if slices.Contains(names[:i], name) {
				return nil, nil, fmt.Errorf("tool %q is listed twice", name)
			}
			if !slices.ContainsFunc(catalog, func(t tools.ToolDescriptor) bool { return t.Name == name }) {
				return nil, nil, fmt.Errorf("tool %q is not in the tool registry", name)
			}
		}
		descriptors = slices.DeleteFunc(slices.Clone(catalog), func(t tools.ToolDescriptor) bool {
			return !slices.Contains(names, t.Name)
		})
	}
	return slices.DeleteFunc(slices.Clone(descriptors), func(t tools.ToolDescriptor) bool { return !checker.Offers(t.Name) }), checker, nil
}
