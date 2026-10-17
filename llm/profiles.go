package llm

import (
	"context"
	"fmt"
)

// ChatModel is a connection that can complete a chat request.
type ChatModel interface {
	Chat(context.Context, ChatRequest, func(Delta)) (ChatResponse, error)
}

// Profiles routes requests to immutable, independently configured clients.
// There is deliberately no fallback: losing one profile must never send its
// conversation to another provider.
type Profiles struct{ clients map[string]ChatModel }

func NewProfiles(clients map[string]ChatModel) *Profiles {
	copy := make(map[string]ChatModel, len(clients))
	for id, client := range clients {
		copy[id] = client
	}
	return &Profiles{clients: copy}
}

func (p *Profiles) Chat(ctx context.Context, req ChatRequest, delta func(Delta)) (ChatResponse, error) {
	id := req.ModelProfileID
	if id == "" {
		id = "primary"
	}
	client, ok := p.clients[id]
	if !ok || client == nil {
		return ChatResponse{}, fmt.Errorf("model profile %q is unavailable; restore its configuration before continuing", id)
	}
	return client.Chat(ctx, req, delta)
}
