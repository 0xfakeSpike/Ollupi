package gateway

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/0xfakeSpike/ollupi/agent"
	"github.com/0xfakeSpike/ollupi/cron"
	"github.com/0xfakeSpike/ollupi/storage"
	"github.com/0xfakeSpike/ollupi/thread"
	"github.com/0xfakeSpike/ollupi/threadmanager"
	"github.com/0xfakeSpike/ollupi/workspace"
)

func TestWorkspaceProtocolAndThreadFiltering(t *testing.T) {
	db, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projects, err := workspace.New(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m, err := threadmanager.NewWithOptions(ctx, catalogTestRunner{}, agent.Agent{Name: "main", Model: "base"}, nil, db, threadmanager.Options{Workspaces: projects})
	if err != nil {
		cancel()
		db.Close()
		t.Fatal(err)
	}
	jobs, err := cron.New(db, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	jobs.SetWorkspaceValidator(projects.RequireActive)
	t.Cleanup(func() { cancel(); m.Wait(); db.Close() })
	c := &conn{s: NewServer(m, jobs, nil, nil, nil), threads: map[string]bool{}, asked: map[string]bool{}}
	if _, _, rpcErr := c.Call(methodWorkspaceList, nil); rpcErr == nil || rpcErr.Code != errNotInitialized {
		t.Fatal("workspace methods bypassed initialization")
	}
	if _, _, rpcErr := c.Call(methodInitialize, []byte(`{"clientInfo":{"name":"test","version":"1"}}`)); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	call := func(method string, params any) json.RawMessage {
		t.Helper()
		raw, _ := json.Marshal(params)
		result, after, rpcErr := c.Call(method, raw)
		if rpcErr != nil {
			t.Fatal(rpcErr)
		}
		if method != methodThreadStart && after != nil {
			after()
		}
		out, _ := json.Marshal(result)
		return out
	}
	raw := call(methodWorkspaceAdd, map[string]any{"path": t.TempDir(), "name": "Project"})
	var result struct {
		Workspace workspace.Workspace `json:"workspace"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	w := result.Workspace
	if w.ID == "" || w.Name != "Project" {
		t.Fatalf("bad workspace contract: %s", raw)
	}
	call(methodThreadStart, map[string]any{"workspaceId": w.ID})
	call(methodThreadStart, map[string]any{})
	for _, test := range []struct {
		params any
		count  int
	}{{map[string]any{}, 2}, {map[string]any{"workspaceId": ""}, 1}, {map[string]any{"workspaceId": w.ID}, 1}} {
		raw := call(methodThreadList, test.params)
		var list threadListResult
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatal(err)
		}
		if len(list.Data) != test.count {
			t.Fatalf("filter %+v returned %s", test.params, raw)
		}
	}
	raw = call(methodCronAdd, map[string]any{"id": "project-job", "spec": "@daily", "prompt": "work", "workspaceId": w.ID})
	var job cron.Job
	if err := json.Unmarshal(raw, &job); err != nil || job.WorkspaceID != w.ID {
		t.Fatalf("cron project contract: %s", raw)
	}
	call(methodWorkspaceRename, map[string]any{"workspaceId": w.ID, "name": "Renamed"})
	call(methodWorkspaceArchive, map[string]any{"workspaceId": w.ID, "archived": true})
	raw = call(methodWorkspaceList, map[string]any{})
	var list struct {
		Data []workspace.Workspace `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil || len(list.Data) != 1 || list.Data[0].Name != "Renamed" || list.Data[0].ArchivedAt == nil {
		t.Fatalf("workspace list did not retain archived metadata: %s", raw)
	}
	params, _ := json.Marshal(map[string]any{"workspaceId": w.ID})
	if _, _, rpcErr := c.Call(methodWorkspaceArchive, params); rpcErr == nil {
		t.Fatal("archive without explicit state accepted")
	}
	if _, _, rpcErr := c.Call(methodThreadStart, params); rpcErr == nil {
		t.Fatal("archived workspace accepted new thread")
	}
	call(methodWorkspaceArchive, map[string]any{"workspaceId": w.ID, "archived": false})
	raw = call(methodThreadStart, map[string]any{"workspaceId": w.ID})
	var started struct {
		Thread thread.Thread `json:"thread"`
	}
	if err := json.Unmarshal(raw, &started); err != nil || started.Thread.WorkspaceID != w.ID {
		t.Fatal("restored project did not accept thread")
	}
}
