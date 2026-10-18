package gateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/0xfakeSpike/ollupi/jsonrpc"
	"github.com/0xfakeSpike/ollupi/memory"
)

type memoryAdminFake struct {
	workspace     string
	includeGlobal bool
	entry         memory.Entry
	forgotten     bool
	conflict      bool
	calls         int
}

func (f *memoryAdminFake) MemoryList(_ context.Context, workspace string, includeGlobal bool) ([]memory.Entry, error) {
	f.workspace, f.includeGlobal = workspace, includeGlobal
	return nil, nil
}
func (f *memoryAdminFake) MemoryStatus(context.Context) (memory.Status, error) {
	return memory.Status{Pending: 3, Active: 2}, nil
}
func (f *memoryAdminFake) MemoryEdit(_ context.Context, id string, revision int, text string) (memory.Entry, error) {
	f.calls++
	if f.conflict || revision != f.entry.Revision {
		return memory.Entry{}, memory.ErrConflict
	}
	f.entry.ID, f.entry.Text, f.entry.Revision = id, text, revision+1
	return f.entry, nil
}
func (f *memoryAdminFake) MemoryForget(_ context.Context, _ string, revision int) error {
	f.calls++
	if f.conflict || revision != f.entry.Revision {
		return memory.ErrConflict
	}
	f.forgotten = true
	return nil
}

func TestMemoryProtocolRequiresInitializationAndKeepsScopesExplicit(t *testing.T) {
	fake := &memoryAdminFake{}
	c := &conn{s: &Server{memory: fake}}
	for _, method := range []string{methodMemoryList, methodMemoryStatus, methodMemoryUpdate, methodMemoryForget} {
		if _, _, err := c.Call(method, nil); err == nil || err.Code != errNotInitialized {
			t.Fatalf("%s bypassed initialization: %v", method, err)
		}
	}
	c.initialized = true
	for _, tc := range []struct {
		params, workspace string
		include           bool
	}{
		{`{}`, "", false}, {`{"workspaceId":"a","includeGlobal":true}`, "a", true}, {`{"workspaceId":"b"}`, "b", false},
	} {
		result, _, err := c.Call(methodMemoryList, json.RawMessage(tc.params))
		if err != nil {
			t.Fatal(err)
		}
		if fake.workspace != tc.workspace || fake.includeGlobal != tc.include {
			t.Fatalf("scope changed: %+v", fake)
		}
		raw, _ := json.Marshal(result)
		if string(raw) != `{"data":[]}` {
			t.Fatalf("empty list wire format: %s", raw)
		}
	}
	result, _, err := c.Call(methodMemoryStatus, nil)
	if err != nil || result.(memory.Status).Pending != 3 {
		t.Fatalf("status: %+v %v", result, err)
	}
}

func TestMemoryMutationsValidateAndExposeRevisionConflict(t *testing.T) {
	fake := &memoryAdminFake{entry: memory.Entry{ID: "one", Revision: 1}}
	c := &conn{s: &Server{memory: fake}, initialized: true}
	for _, params := range []string{`{"id":"one","text":"valid"}`, `{"id":"one","revision":1,"text":" "}`, `{"revision":1,"text":"valid"}`} {
		if _, _, err := c.Call(methodMemoryUpdate, json.RawMessage(params)); err == nil || err.Code != jsonrpc.CodeInvalidParams {
			t.Fatalf("accepted invalid edit %s: %v", params, err)
		}
	}
	if fake.calls != 0 {
		t.Fatal("invalid parameters reached storage")
	}
	result, _, err := c.Call(methodMemoryUpdate, json.RawMessage(`{"id":"one","revision":1,"text":" corrected "}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	var updated struct {
		Memory memory.Entry `json:"memory"`
	}
	if err := json.Unmarshal(raw, &updated); err != nil || updated.Memory.Text != "corrected" || updated.Memory.Revision != 2 {
		t.Fatalf("edit: %s, %v", raw, err)
	}
	if _, _, err := c.Call(methodMemoryForget, json.RawMessage(`{"id":"one","revision":1}`)); err == nil || err.Code != errMemoryConflict {
		t.Fatalf("stale revision: %v", err)
	}
	if fake.forgotten {
		t.Fatal("stale client forgot updated memory")
	}
	if _, _, err := c.Call(methodMemoryForget, json.RawMessage(`{"id":"one","revision":2}`)); err != nil || !fake.forgotten {
		t.Fatalf("forget: %v", err)
	}
}

func TestMemoryProtocolUnavailable(t *testing.T) {
	c := &conn{s: &Server{}, initialized: true}
	if _, _, err := c.Call(methodMemoryList, nil); err == nil || err.Code != jsonrpc.CodeServerError {
		t.Fatalf("missing store: %v", err)
	}
}

func TestCronProtocolUnavailable(t *testing.T) {
	c := &conn{s: &Server{}, initialized: true}
	for _, m := range []string{methodCronList, methodCronAdd, methodCronRemove, methodCronRun} {
		if _, _, err := c.Call(m, json.RawMessage(`{"id":"j","spec":"@daily","prompt":"p"}`)); err == nil || err.Code != jsonrpc.CodeServerError {
			t.Fatalf("%s without a scheduler: %v", m, err)
		}
	}
}
