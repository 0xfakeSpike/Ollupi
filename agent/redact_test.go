package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/0xfakeSpike/ollupi/agent/contextmanager"
	"github.com/0xfakeSpike/ollupi/llm"
)

// TestSendRedactsSystemReasoningAndToolCalls guards the model boundary: the
// system prompt, message content, reasoning, and the tool-call arguments of
// the replayed history must all be masked before they reach the model.
func TestSendRedactsSystemReasoningAndToolCalls(t *testing.T) {
	var captured llm.ChatRequest
	rn, r, _ := statsFixture(t, func(_ context.Context, req llm.ChatRequest, _ func(llm.Delta)) (llm.ChatResponse, error) {
		captured = req
		return llm.ChatResponse{Message: llm.ChatMessage{Role: llm.RoleAssistant, Content: "ok"}}, nil
	})

	prompt := contextmanager.Prompt{
		System: "系统提示里的号码 13800000001",
		Messages: []contextmanager.Message{
			{Role: contextmanager.RoleUser, Content: "用户说 13800000002"},
			{
				Role:      contextmanager.RoleAssistant,
				Content:   "助手回复",
				Reasoning: "推理里提到 13800000003",
				ToolCalls: []contextmanager.ToolCall{{ID: "c1", Name: "survey_call", Arguments: `{"phone":"13800000004"}`}},
			},
		},
	}

	if _, err := rn.send(context.Background(), r, prompt, llm.ToolChoiceAuto, nil); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(captured)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, secret := range []string{"13800000001", "13800000002", "13800000003", "13800000004"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redaction leaked %s into the model request: %s", secret, got)
		}
	}
	if !strings.Contains(got, "PHONE:") {
		t.Fatalf("expected a PHONE placeholder in the model request: %s", got)
	}
}
