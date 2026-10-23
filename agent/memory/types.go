package memory

import (
	"encoding/json"
	"errors"

	"github.com/0xfakeSpike/ollupi/repo"
)

var ErrConflict = errors.New("memory changed while processing; retry")

type Fact struct {
	Source repo.MemorySource `json:"source"`
	Data   json.RawMessage   `json:"data"`
}

type Batch struct {
	WorkspaceID string                 `json:"workspaceId"`
	Changes     []repo.MemoryChangeRow `json:"-"`
	Facts       []Fact                 `json:"facts"`
	Existing    []repo.MemoryRow       `json:"existing"`
}

type Operation struct {
	Action   string              `json:"action"`
	ID       string              `json:"id,omitempty"`
	Revision int                 `json:"revision,omitempty"`
	Topic    string              `json:"topic,omitempty"`
	Kind     string              `json:"kind,omitempty"`
	Text     string              `json:"text,omitempty"`
	Sources  []repo.MemorySource `json:"sources"`
}
