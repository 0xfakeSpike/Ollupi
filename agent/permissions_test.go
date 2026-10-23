package agent

import (
	"context"
	"testing"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/permission"
	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/agent/tools/subagent"
	"github.com/0xfakeSpike/ollupi/repo"
)

func TestPolicyAndRulesResolution(t *testing.T) {
	db := openAgentTestDB(t)
	now := time.Now().UTC()
	ws := repo.WorkspaceRow{
		ID: "ws1", Name: "proj", RootPath: "/proj",
		ApprovalPolicy: "never",
		Permissions:    repo.PermissionRules{Allow: []string{"shell"}},
		CreatedAt:      now, UpdatedAt: now,
	}
	if _, err := db.AddWorkspace(ws, func(existing repo.WorkspaceRow) bool { return existing.ID == ws.ID }, nil); err != nil {
		t.Fatalf("add workspace: %v", err)
	}
	a := &Agent{
		DB:             db,
		ApprovalPolicy: permission.OnRequest,
		Permissions:    permission.Rules{Deny: []string{"shell"}},
	}

	inWorkspace := repo.ThreadRow{ID: "t1", WorkspaceID: "ws1"}
	if got := a.policyFor(inWorkspace); got != permission.Never {
		t.Fatalf("workspace policy not applied: %q", got)
	}
	if rules := a.rulesFor(inWorkspace); len(rules.Allow) != 1 || rules.Allow[0] != "shell" || len(rules.Deny) != 0 {
		t.Fatalf("workspace rules not applied: %+v", rules)
	}

	noWorkspace := repo.ThreadRow{ID: "t2", WorkspaceID: "missing"}
	if got := a.policyFor(noWorkspace); got != permission.OnRequest {
		t.Fatalf("agent default policy not applied: %q", got)
	}
	if rules := a.rulesFor(noWorkspace); len(rules.Deny) != 1 || rules.Deny[0] != "shell" {
		t.Fatalf("agent default rules not applied: %+v", rules)
	}

	overridden := repo.ThreadRow{ID: "t3", WorkspaceID: "ws1", ApprovalPolicy: "on-request"}
	if got := a.policyFor(overridden); got != permission.OnRequest {
		t.Fatalf("thread override not applied: %q", got)
	}

	child := repo.ThreadRow{ID: "t4", WorkspaceID: "ws1", ParentThreadID: "t1"}
	if rules := a.rulesFor(child); !contains(rules.Deny, subagent.Tool) {
		t.Fatalf("subagent tool not denied: %+v", rules)
	}
}

func TestValidateWorkspaceSettings(t *testing.T) {
	registry, err := tools.New(context.Background(), toolProvider{specs: []*tools.Spec{{
		Name: "shell", Description: "run commands", InputSchema: map[string]any{"type": "object"},
	}}})
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	a := &Agent{ApprovalPolicy: permission.OnRequest, registry: registry}
	if err := a.ValidateWorkspaceSettings("never", repo.PermissionRules{Allow: []string{"shell"}}); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
	if err := a.ValidateWorkspaceSettings("bogus", repo.PermissionRules{}); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if err := a.ValidateWorkspaceSettings("", repo.PermissionRules{Deny: []string{"no-such-tool"}}); err == nil {
		t.Fatal("rule for an unknown tool accepted")
	}
}

func openAgentTestDB(t *testing.T) *repo.DB {
	t.Helper()
	db, err := repo.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

type toolProvider struct {
	specs []*tools.Spec
}

func (p toolProvider) Name() (string, string) { return "test", "test tools" }

func (p toolProvider) ToolList(context.Context) ([]*tools.Spec, error) { return p.specs, nil }

func (p toolProvider) Invoke(context.Context, string, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "ok"}, nil
}
