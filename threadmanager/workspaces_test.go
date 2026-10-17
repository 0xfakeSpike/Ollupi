package threadmanager

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xfakeSpike/ollupi/agent"
	execcontext "github.com/0xfakeSpike/ollupi/execution"
	"github.com/0xfakeSpike/ollupi/storage"
	"github.com/0xfakeSpike/ollupi/thread"
	"github.com/0xfakeSpike/ollupi/workspace"
)

func workspaceFixture(t *testing.T, db *storage.DB, r Runner, opts Options, subagents ...agent.Agent) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ag := agent.Agent{Name: "main", Model: "base", ModelProfileID: "primary", ApprovalPolicy: agent.ApprovalOnRequest}
	for _, child := range subagents {
		ag.Subagents = append(ag.Subagents, agent.Subagent{Name: child.Name, Description: "test"})
	}
	m, err := NewWithOptions(ctx, r, ag, subagents, db, opts, availableModels()...)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); m.Wait() })
	return m
}

func workspaceOptions(t *testing.T, db *storage.DB) Options {
	t.Helper()
	projects, err := workspace.New(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	general, err := execcontext.CanonicalDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Options{Workspaces: projects, ValidateExecution: execcontext.Validate, ResolveExecution: func(th thread.Thread) (*execcontext.Context, error) {
		root, cwd := "", general
		if th.WorkspaceID != "" {
			w, ok := projects.Get(th.WorkspaceID)
			if !ok {
				t.Fatalf("unknown workspace %s", th.WorkspaceID)
			}
			root, cwd = w.RootPath, w.RootPath
		}
		var rules []execcontext.Instruction
		if data, err := os.ReadFile(filepath.Join(cwd, "AGENTS.md")); err == nil {
			rules = append(rules, execcontext.Instruction{Scope: "project", Path: filepath.Join(cwd, "AGENTS.md"), Content: string(data)})
		}
		return &execcontext.Context{Version: execcontext.Version, RootPath: root, DefaultCwd: cwd, CwdRoots: []string{cwd}, Sandbox: "unsandboxed", SandboxDigest: "sha256:fixture", Instructions: rules}, nil
	}}
}

func addProjectThread(t *testing.T, m *Manager, path string) (workspace.Workspace, thread.Thread) {
	t.Helper()
	w, err := m.AddWorkspace(path, "")
	if err != nil {
		t.Fatal(err)
	}
	th, err := m.StartThread(StartThreadParams{Source: thread.SourceGateway, WorkspaceID: w.ID})
	if err != nil {
		t.Fatal(err)
	}
	return w, th
}

func TestConcurrentProjectsHaveIndependentExecutionSnapshots(t *testing.T) {
	db := modelDB(t)
	opts := workspaceOptions(t, db)
	seen := make(chan struct {
		workspace string
		snapshot  *execcontext.Context
	}, 3)
	release := make(chan struct{})
	r := &modelRunner{run: func(ctx context.Context, _ agent.Agent, turn agent.Turn, _ agent.Recorder) (agent.Result, error) {
		seen <- struct {
			workspace string
			snapshot  *execcontext.Context
		}{execcontext.WorkspaceID(ctx), execcontext.Clone(turn.ExecutionContext)}
		select {
		case <-release:
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		}
		return agent.Result{}, nil
	}}
	m := workspaceFixture(t, db, r, opts)
	first, a := addProjectThread(t, m, t.TempDir())
	second, b := addProjectThread(t, m, t.TempDir())
	general := createModelThread(t, m)
	for _, th := range []thread.Thread{a, b, general} {
		turn, err := m.StartTurn(th.ID, "work")
		if err != nil {
			t.Fatal(err)
		}
		if turn.ExecutionContext == nil {
			t.Fatal("new turn has no execution snapshot")
		}
	}
	got := map[string]*execcontext.Context{}
	for i := 0; i < 3; i++ {
		select {
		case run := <-seen:
			got[run.workspace] = run.snapshot
		case <-time.After(3 * time.Second):
			t.Fatal("projects did not run concurrently")
		}
	}
	close(release)
	m.Wait()
	if got[first.ID].RootPath != first.RootPath || got[second.ID].RootPath != second.RootPath || got[""].RootPath != "" {
		t.Fatalf("project state crossed conversations: %+v", got)
	}
	empty := ""
	if len(m.ListThreads(thread.Filter{})) != 3 || len(m.ListThreads(thread.Filter{WorkspaceID: &empty})) != 1 || len(m.ListThreads(thread.Filter{WorkspaceID: &first.ID})) != 1 {
		t.Fatal("workspace filter did not distinguish all/general/project")
	}
	// The snapshot returned to a caller cannot mutate durable or in-memory state.
	view, _ := m.ReadThread(a.ID)
	view.Turns[0].ExecutionContext.DefaultCwd = second.RootPath
	again, _ := m.ReadThread(a.ID)
	if again.Turns[0].ExecutionContext.DefaultCwd != first.RootPath {
		t.Fatal("snapshot escaped without cloning")
	}
}

func TestApprovalRestartUsesSavedRulesAndArchiveAllowsCompletion(t *testing.T) {
	db := modelDB(t)
	opts := workspaceOptions(t, db)
	r := approvalRunner()
	m := workspaceFixture(t, db, r, opts)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("original rules"), 0600); err != nil {
		t.Fatal(err)
	}
	w, th := addProjectThread(t, m, root)
	original, err := m.StartTurn(th.ID, "approve")
	if err != nil {
		t.Fatal(err)
	}
	m.Wait()
	if _, err := m.ArchiveWorkspace(w.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.StartThread(StartThreadParams{Source: thread.SourceGateway, WorkspaceID: w.ID}); err == nil {
		t.Fatal("new thread in archived workspace accepted")
	}
	if _, err := m.SubmitCronRun("job", "prompt", w.ID); err == nil {
		t.Fatal("archived cron run accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("changed rules"), 0600); err != nil {
		t.Fatal(err)
	}
	var resumed *execcontext.Context
	r2 := &modelRunner{run: func(_ context.Context, _ agent.Agent, turn agent.Turn, _ agent.Recorder) (agent.Result, error) {
		resumed = execcontext.Clone(turn.ExecutionContext)
		return agent.Result{}, nil
	}}
	restarted := workspaceFixture(t, db, r2, opts)
	if len(r2.recorded()) != 0 {
		t.Fatal("pending approval ran before user replied")
	}
	if err := restarted.ResolveApproval(th.ID, "approval-id", thread.DecisionAccept, "test"); err != nil {
		t.Fatal(err)
	}
	restarted.Wait()
	if !reflect.DeepEqual(resumed, original.ExecutionContext) || resumed.Instructions[0].Content != "original rules" {
		t.Fatalf("restart replaced saved rules: %+v", resumed)
	}
	if _, err := restarted.StartTurn(th.ID, "new"); err == nil {
		t.Fatal("archived workspace accepted new turn")
	}
	if _, err := restarted.ArchiveWorkspace(w.ID, false); err != nil {
		t.Fatal(err)
	}
	next, err := restarted.StartTurn(th.ID, "new")
	if err != nil {
		t.Fatal(err)
	}
	restarted.Wait()
	if next.ExecutionContext.Instructions[0].Content != "changed rules" {
		t.Fatal("next turn did not refresh project instructions")
	}
	cronThread, err := restarted.SubmitCronRun("job", "prompt", w.ID)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Wait()
	if cronThread.WorkspaceID != w.ID {
		t.Fatal("cron run lost project")
	}
}

func TestLegacyUnfinishedTurnIsInterruptedWithoutGuessingDirectory(t *testing.T) {
	db := modelDB(t)
	old := modelFixture(t, db, approvalRunner(), nil, availableModels()...)
	th := createModelThread(t, old)
	if _, err := old.StartTurn(th.ID, "pending before upgrade"); err != nil {
		t.Fatal(err)
	}
	old.Wait()
	r := &modelRunner{}
	current := workspaceFixture(t, db, r, workspaceOptions(t, db))
	current.Wait()
	view, _ := current.ReadThread(th.ID)
	if len(r.recorded()) != 0 || view.Turns[0].Status != thread.TurnInterrupted || view.Turns[0].ExecutionContext != nil || !strings.Contains(view.Turns[0].Error, "original working directory") {
		t.Fatalf("legacy run guessed directory: %+v", view.Turns)
	}
	if len(view.Items) != 2 || view.Items[0].Text != "pending before upgrade" || view.Items[1].Approval == nil {
		t.Fatal("legacy records were removed")
	}
	if _, err := current.StartTurn(th.ID, "fresh run"); err != nil {
		t.Fatal(err)
	}
	current.Wait()
	view, _ = current.ReadThread(th.ID)
	if view.Turns[1].ExecutionContext == nil {
		t.Fatal("new general turn did not gain snapshot")
	}
}

func TestUnavailableDirectoryBlocksApprovalAndRestart(t *testing.T) {
	db := modelDB(t)
	opts := workspaceOptions(t, db)
	m := workspaceFixture(t, db, approvalRunner(), opts)
	root := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	_, th := addProjectThread(t, m, root)
	if _, err := m.StartTurn(th.ID, "pending"); err != nil {
		t.Fatal(err)
	}
	m.Wait()
	if err := os.Rename(root, root+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := m.ResolveApproval(th.ID, "approval-id", thread.DecisionAccept, "test"); err == nil {
		t.Fatal("approval accepted for missing directory")
	}
	r := &modelRunner{}
	restarted := workspaceFixture(t, db, r, opts)
	restarted.Wait()
	view, _ := restarted.ReadThread(th.ID)
	if view.Turns[0].Status != thread.TurnInterrupted || len(r.recorded()) != 0 {
		t.Fatal("missing project automatically ran")
	}
}

func TestSubagentInheritsWorkspaceAndExactParentSnapshot(t *testing.T) {
	db := modelDB(t)
	opts := workspaceOptions(t, db)
	main := agent.Agent{Name: "main", Model: "base", ModelProfileID: "primary"}
	child := (agent.Definition{Name: "child", Instructions: "test"}).Agent(main)
	var mu sync.Mutex
	var snapshots []*execcontext.Context
	var owners []string
	r := &modelRunner{run: func(ctx context.Context, a agent.Agent, turn agent.Turn, rec agent.Recorder) (agent.Result, error) {
		mu.Lock()
		snapshots = append(snapshots, execcontext.Clone(turn.ExecutionContext))
		owners = append(owners, execcontext.WorkspaceID(ctx))
		mu.Unlock()
		now := time.Now().UTC()
		if a.Name == "child" {
			_, err := rec.Record(thread.Item{ID: turn.ID + "answer", TurnID: turn.ID, Type: thread.ItemAgentMessage, Status: thread.ItemCompleted, Text: "done", StartedAt: now, CompletedAt: &now})
			return agent.Result{}, err
		}
		for _, it := range turn.Items {
			if it.Type == thread.ItemCollabAgentToolCall {
				return agent.Result{}, nil
			}
		}
		// A project rule edited during the parent turn must not affect its child.
		if err := os.WriteFile(filepath.Join(turn.ExecutionContext.RootPath, "AGENTS.md"), []byte("changed during parent"), 0600); err != nil {
			return agent.Result{}, err
		}
		_, err := rec.Record(thread.Item{ID: "delegate", TurnID: turn.ID, Type: thread.ItemCollabAgentToolCall, Status: thread.ItemInProgress, Agent: "child", CallID: "delegate", Tool: agent.ToolAgent, Arguments: `{"subagent_type":"child","description":"review","prompt":"work"}`, StartedAt: now})
		return agent.Result{Waiting: true}, err
	}}
	m := workspaceFixture(t, db, r, opts, child)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("parent rules"), 0600); err != nil {
		t.Fatal(err)
	}
	w, th := addProjectThread(t, m, root)
	initial, err := m.StartTurn(th.ID, "delegate")
	if err != nil {
		t.Fatal(err)
	}
	m.Wait()
	if len(snapshots) != 3 {
		t.Fatalf("expected parent,child,parent, got %d", len(snapshots))
	}
	for i, snapshot := range snapshots {
		if !reflect.DeepEqual(snapshot, initial.ExecutionContext) || owners[i] != w.ID {
			t.Fatalf("child changed execution environment: %+v %q", snapshot, owners[i])
		}
	}
	views, err := db.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.Thread.WorkspaceID != w.ID {
			t.Fatalf("persisted child lost workspace: %+v", view.Thread)
		}
	}
}

func TestRunningProjectTurnRecoversSavedExecutionSnapshot(t *testing.T) {
	db := modelDB(t)
	opts := workspaceOptions(t, db)
	started := make(chan struct{})
	r := &modelRunner{run: func(ctx context.Context, _ agent.Agent, _ agent.Turn, _ agent.Recorder) (agent.Result, error) {
		close(started)
		<-ctx.Done()
		return agent.Result{}, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	m, err := NewWithOptions(ctx, r, agent.Agent{Name: "main", Model: "base", ModelProfileID: "primary"}, nil, db, opts, availableModels()...)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); m.Wait() })
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("before shutdown"), 0600); err != nil {
		t.Fatal(err)
	}
	w, th := addProjectThread(t, m, root)
	original, err := m.StartTurn(th.ID, "work")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not start")
	}
	cancel()
	m.Wait()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("after shutdown"), 0600); err != nil {
		t.Fatal(err)
	}
	var recovered *execcontext.Context
	var workspaceID string
	r2 := &modelRunner{run: func(ctx context.Context, _ agent.Agent, turn agent.Turn, _ agent.Recorder) (agent.Result, error) {
		recovered = execcontext.Clone(turn.ExecutionContext)
		workspaceID = execcontext.WorkspaceID(ctx)
		return agent.Result{}, nil
	}}
	restarted := workspaceFixture(t, db, r2, opts)
	restarted.Wait()
	if !reflect.DeepEqual(recovered, original.ExecutionContext) || workspaceID != w.ID {
		t.Fatalf("running recovery changed execution: %+v %s", recovered, workspaceID)
	}
	view, _ := restarted.ReadThread(th.ID)
	if view.Turns[0].Status != thread.TurnCompleted {
		t.Fatal("running turn did not complete after recovery")
	}
}
