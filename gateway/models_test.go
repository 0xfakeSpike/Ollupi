package gateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/0xfakeSpike/ollupi/agent"
	"github.com/0xfakeSpike/ollupi/storage"
	"github.com/0xfakeSpike/ollupi/thread"
	"github.com/0xfakeSpike/ollupi/threadmanager"
)

type catalogTestRunner struct{}

func (catalogTestRunner) Run(context.Context, agent.Agent, agent.Turn, agent.Recorder) (agent.Result, error) {
	return agent.Result{}, nil
}

func TestModelProtocolListsMetadataAndStartsSelectedProfile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	db, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := threadmanager.New(ctx, catalogTestRunner{}, agent.Agent{Name: "test", Model: "base", ModelProfileID: "primary"}, nil, db,
		threadmanager.ModelProfile{ID: "primary", Name: "Primary connection", Model: "base"},
		threadmanager.ModelProfile{ID: "other", Name: "Other", Model: "other-model"})
	if err != nil {
		cancel()
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); m.Wait(); db.Close() })
	c := &conn{s: NewServer(m, nil, nil, nil, nil), threads: map[string]bool{}, asked: map[string]bool{}}
	if _, _, rpcErr := c.Call(methodModelList, nil); rpcErr == nil || rpcErr.Code != errNotInitialized {
		t.Fatalf("catalog bypassed initialization: %v", rpcErr)
	}
	if _, _, rpcErr := c.Call(methodInitialize, []byte(`{"clientInfo":{"name":"test","version":"1"}}`)); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	result, _, rpcErr := c.Call(methodModelList, nil)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	raw, _ := json.Marshal(result)
	var catalog map[string]json.RawMessage
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog) != 2 || string(catalog["defaultModelProfileId"]) != `"primary"` {
		t.Fatalf("catalog contract: %s", raw)
	}
	var data []map[string]any
	if err := json.Unmarshal(catalog["data"], &data); err != nil {
		t.Fatal(err)
	}
	if len(data) != 2 || len(data[0]) != 3 || data[1]["id"] != "other" || data[1]["model"] != "other-model" {
		t.Fatalf("catalog exposed extra fields or lost metadata: %s", raw)
	}
	th, err := m.StartThread(threadmanager.StartThreadParams{Source: thread.SourceGateway})
	if err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]any{"threadId": th.ID, "input": []map[string]string{{"type": "text", "text": "hello"}}, "modelProfileId": "other"})
	result, _, rpcErr = c.Call(methodTurnStart, params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	m.Wait()
	turn := result.(turnResult).Turn
	if turn.ModelProfileID != "other" || turn.Model != "other-model" {
		t.Fatalf("selection lost in gateway: %+v", turn)
	}
	params, _ = json.Marshal(map[string]any{"threadId": th.ID, "input": []map[string]string{{"type": "text", "text": "bad"}}, "modelProfileId": "missing"})
	if _, _, rpcErr = c.Call(methodTurnStart, params); rpcErr == nil {
		t.Fatal("unknown selection accepted")
	}
	view, _ := m.ReadThread(th.ID)
	if len(view.Turns) != 1 {
		t.Fatal("rejected selection created a turn")
	}
}
