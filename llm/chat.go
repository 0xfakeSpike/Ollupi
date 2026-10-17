// Package llm is a client for OpenAI-compatible chat completion endpoints
// with native tool calling and streamed responses.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	openai "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/respjson"
	"github.com/openai/openai-go/shared"
)

// Role is a chat message author.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ChatMessage is one provider-neutral chat message. Assistant messages may
// carry tool calls; tool messages answer exactly one earlier tool call.
//
// ReasoningContent is the provider's "reasoning_content" extension used by
// thinking models (Qwen, DeepSeek). Some providers require it to be sent back
// on assistant messages that carry tool calls, so it is round-tripped as is.
type ChatMessage struct {
	Role             Role
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	ToolCallID       string
}

// ToolCall is one function call requested by the model. Arguments is the raw
// JSON text produced by the model and must be validated by the caller.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ToolSpec is one function definition offered to the model.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]any
}

type ToolChoice string

const (
	ToolChoiceAuto ToolChoice = "auto"
	ToolChoiceNone ToolChoice = "none"
)

type ChatRequest struct {
	Model          string
	ModelProfileID string
	// EnableThinking, when non-nil, is sent as the provider's
	// "enable_thinking" extension (Qwen/DashScope).
	EnableThinking *bool
	Messages       []ChatMessage
	Tools          []ToolSpec
	ToolChoice     ToolChoice
	// ParallelToolCalls lets the model return several tool calls in one
	// response. Off, it returns at most one.
	ParallelToolCalls bool
}

type Usage struct {
	PromptTokens       int64
	CachedPromptTokens int64
	CompletionTokens   int64
}

type ChatResponse struct {
	Message      ChatMessage
	FinishReason string
	Usage        Usage
	// UsageReported distinguishes a provider's explicit zero usage from an
	// omitted usage chunk. Reported usage survives an otherwise failed stream.
	UsageReported bool
}

// defaultChatTimeout bounds one whole streamed response.
const defaultChatTimeout = 180 * time.Second

// OpenAI is a client for an OpenAI-compatible chat completions endpoint.
type OpenAI struct {
	client openai.Client
}

// NewOpenAI returns a client for an OpenAI-compatible endpoint. The model is
// named per request.
func NewOpenAI(baseURL, apiKey string) *OpenAI {
	// An empty key intentionally means no credential. Never inherit an
	// environment key into a different provider's endpoint.
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if apiKey == "" {
		opts = append(opts, option.WithHeaderDel("Authorization"))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &OpenAI{client: openai.NewClient(opts...)}
}

// withDefaultTimeout applies timeout unless ctx already has a deadline.
func withDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// Delta is a piece of a response as it streams in: text of the answer, or
// text of the provider's reasoning.
type Delta struct {
	Content   string
	Reasoning string
}

// Chat performs one native tool-calling completion, streamed. onDelta, when
// non-nil, receives answer and reasoning text as it arrives; the returned
// response holds the whole message. Parallel tool calls are off unless the
// request turns them on.
func (c *OpenAI) Chat(ctx context.Context, req ChatRequest, onDelta func(Delta)) (ChatResponse, error) {
	if req.Model == "" {
		return ChatResponse{}, fmt.Errorf("llm.OpenAI: model required")
	}
	ctx, cancel := withDefaultTimeout(ctx, defaultChatTimeout)
	defer cancel()

	if req.ToolChoice != ToolChoiceAuto && req.ToolChoice != ToolChoiceNone {
		return ChatResponse{}, fmt.Errorf("llm.OpenAI: unsupported tool choice %q", req.ToolChoice)
	}
	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(req.Model),
		Messages: make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)),
		// Usage arrives in a final chunk with no choices.
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	}
	for i, message := range req.Messages {
		converted, err := toMessageParam(message)
		if err != nil {
			return ChatResponse{}, fmt.Errorf("llm.OpenAI: message %d: %w", i, err)
		}
		params.Messages = append(params.Messages, converted)
	}
	if len(req.Tools) > 0 {
		params.Tools = make([]openai.ChatCompletionToolParam, 0, len(req.Tools))
		for _, tool := range req.Tools {
			params.Tools = append(params.Tools, openai.ChatCompletionToolParam{
				Function: shared.FunctionDefinitionParam{
					Name:        tool.Name,
					Description: openai.String(tool.Description),
					Parameters:  shared.FunctionParameters(tool.Parameters),
				},
			})
		}
		params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String(string(req.ToolChoice))}
		params.ParallelToolCalls = openai.Bool(req.ParallelToolCalls)
	}

	var requestOptions []option.RequestOption
	if req.EnableThinking != nil {
		requestOptions = append(requestOptions, option.WithJSONSet("enable_thinking", *req.EnableThinking))
	}
	// This SDK version appends per-request options to the service's slice.
	// Give each concurrent turn its own backing array so thinking options
	// cannot race or leak into another request on the same connection.
	completions := c.client.Chat.Completions
	completions.Options = append([]option.RequestOption(nil), completions.Options...)
	stream := completions.NewStreaming(ctx, params, requestOptions...)
	defer stream.Close()
	var acc streamAccumulator
	for stream.Next() {
		delta, err := acc.add(stream.Current())
		if err != nil {
			return acc.usageResponse(), err
		}
		if onDelta != nil && (delta.Content != "" || delta.Reasoning != "") {
			onDelta(delta)
		}
	}
	if err := stream.Err(); err != nil {
		return acc.usageResponse(), safeRequestError(err)
	}
	return acc.response()
}

// SDK errors can contain response bodies and request URLs. Neither belongs
// in persisted conversation errors: a provider might echo credentials.
func safeRequestError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return fmt.Errorf("model request failed (HTTP %d); check the selected connection and model", apiErr.StatusCode)
	}
	return fmt.Errorf("model request failed; check the selected connection and try again")
}

// streamAccumulator assembles a streamed response. Only the first choice is
// used; tool calls are assembled by their index.
type streamAccumulator struct {
	sawChoice     bool
	content       strings.Builder
	reasoning     strings.Builder
	finish        string
	calls         []ToolCall
	usage         Usage
	usageReported bool
}

func (a *streamAccumulator) add(chunk openai.ChatCompletionChunk) (Delta, error) {
	if chunk.JSON.Usage.Valid() {
		a.usageReported = true
		a.usage = Usage{
			PromptTokens:       chunk.Usage.PromptTokens,
			CachedPromptTokens: chunk.Usage.PromptTokensDetails.CachedTokens,
			CompletionTokens:   chunk.Usage.CompletionTokens,
		}
	}
	if len(chunk.Choices) == 0 {
		return Delta{}, nil
	}
	a.sawChoice = true
	choice := chunk.Choices[0]
	if choice.FinishReason != "" {
		a.finish = choice.FinishReason
	}
	reasoning, err := extraString(choice.Delta.JSON.ExtraFields, "reasoning_content")
	if err != nil {
		return Delta{}, err
	}
	a.content.WriteString(choice.Delta.Content)
	a.reasoning.WriteString(reasoning)
	for _, part := range choice.Delta.ToolCalls {
		i := int(part.Index)
		if i < 0 || i > len(a.calls) {
			return Delta{}, fmt.Errorf("llm.OpenAI: tool call index %d out of order", part.Index)
		}
		if i == len(a.calls) {
			a.calls = append(a.calls, ToolCall{})
		}
		call := &a.calls[i]
		if part.ID != "" {
			call.ID = part.ID
		}
		call.Name += part.Function.Name
		call.Arguments += part.Function.Arguments
	}
	return Delta{Content: choice.Delta.Content, Reasoning: reasoning}, nil
}

func (a *streamAccumulator) response() (ChatResponse, error) {
	if !a.sawChoice {
		return a.usageResponse(), fmt.Errorf("llm.OpenAI: empty completion choices")
	}
	// A complete response always ends with a finish reason; without one the
	// stream was cut off and the message is partial.
	if a.finish == "" {
		return a.usageResponse(), fmt.Errorf("llm.OpenAI: the stream ended before the response finished")
	}
	for _, call := range a.calls {
		if call.ID == "" || call.Name == "" {
			return a.usageResponse(), fmt.Errorf("llm.OpenAI: tool call without id or function name")
		}
	}
	return ChatResponse{
		Message:       ChatMessage{Role: RoleAssistant, Content: a.content.String(), ReasoningContent: a.reasoning.String(), ToolCalls: a.calls},
		FinishReason:  a.finish,
		Usage:         a.usage,
		UsageReported: a.usageReported,
	}, nil
}

// usageResponse retains billing metadata without exposing an incomplete
// message or tool call as a usable model response.
func (a *streamAccumulator) usageResponse() ChatResponse {
	return ChatResponse{Usage: a.usage, UsageReported: a.usageReported}
}

// extraString reads a provider extension field such as "reasoning_content".
// The SDK reports untyped extra fields as not Valid even when present, so the
// raw JSON is decoded directly; absent and null both mean empty.
func extraString(fields map[string]respjson.Field, name string) (string, error) {
	field, ok := fields[name]
	if !ok || field.Raw() == "" || field.Raw() == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal([]byte(field.Raw()), &text); err != nil {
		return "", fmt.Errorf("llm.OpenAI: decode %s: %w", name, err)
	}
	return text, nil
}

func toMessageParam(message ChatMessage) (openai.ChatCompletionMessageParamUnion, error) {
	switch message.Role {
	case RoleSystem:
		return openai.SystemMessage(message.Content), nil
	case RoleUser:
		return openai.UserMessage(message.Content), nil
	case RoleAssistant:
		var assistant openai.ChatCompletionAssistantMessageParam
		if message.Content != "" {
			assistant.Content.OfString = openai.String(message.Content)
		}
		for _, call := range message.ToolCalls {
			assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallParam{
				ID: call.ID,
				Function: openai.ChatCompletionMessageToolCallFunctionParam{
					Name:      call.Name,
					Arguments: call.Arguments,
				},
			})
		}
		if message.ReasoningContent != "" {
			assistant.SetExtraFields(map[string]any{"reasoning_content": message.ReasoningContent})
		}
		return openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant}, nil
	case RoleTool:
		if message.ToolCallID == "" {
			return openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("tool message requires tool_call_id")
		}
		return openai.ToolMessage(message.Content, message.ToolCallID), nil
	default:
		return openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("unsupported role %q", message.Role)
	}
}
