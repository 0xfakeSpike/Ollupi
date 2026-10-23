package memory

import (
	"encoding/json"

	"github.com/0xfakeSpike/ollupi/repo"
)

type Fact struct {
	Source repo.MemorySource `json:"source"`
	Data   json.RawMessage   `json:"data"`
}

type Batch struct {
	WorkspaceID string                `json:"workspaceId"`
	Events      []repo.MemoryEventRow `json:"-"`
	Facts       []Fact                `json:"facts"`
	Existing    []repo.MemoryRow      `json:"existing"`
}

type Operation struct {
	Action  string              `json:"action"`
	ID      string              `json:"id,omitempty"`
	Topic   string              `json:"topic,omitempty"`
	Kind    string              `json:"kind,omitempty"`
	Text    string              `json:"text,omitempty"`
	Sources []repo.MemorySource `json:"sources"`
}

// Context is the memory snapshot a turn receives. It is transport-agnostic;
// the runtime turns it into a memoryContext item.
type Context struct {
	Entries []repo.MemoryReference
	Text    string
}
