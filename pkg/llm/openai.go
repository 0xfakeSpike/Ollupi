package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/respjson"
	"github.com/openai/openai-go/shared"
)

const defaultChatTimeout = 180 * time.Second

type OpenAI struct {
	client openai.Client
	limits map[string]Limits
}

// NewOpenAI connects to an OpenAI-compatible endpoint. limits declares, by
// exact model name, the limits of models this endpoint serves; they take
// precedence over the known ones (see Limits). It may be nil.
func NewOpenAI(baseURL, apiKey string, limits map[string]Limits) *OpenAI {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if apiKey == "" {
		opts = append(opts, option.WithHeaderDel("Authorization"))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &OpenAI{client: openai.NewClient(opts...), limits: maps.Clone(limits)}
}

func withDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func (c *OpenAI) Chat(ctx context.Context, req ChatRequest, onDelta func(Delta)) (ChatResponse, error) {
	if req.Model.Name == "" {
		return ChatResponse{}, fmt.Errorf("llm.OpenAI: model required")
	}
	ctx, cancel := withDefaultTimeout(ctx, defaultChatTimeout)
	defer cancel()

	if req.ToolChoice != ToolChoiceAuto && req.ToolChoice != ToolChoiceNone {
		return ChatResponse{}, fmt.Errorf("llm.OpenAI: unsupported tool choice %q", req.ToolChoice)
	}
	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(req.Model.Name),
		Messages: make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)),

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
	if req.Model.Thinking != nil {
		requestOptions = append(requestOptions, option.WithJSONSet("enable_thinking", *req.Model.Thinking))
	}

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

func (a *streamAccumulator) usageResponse() ChatResponse {
	return ChatResponse{Usage: a.usage, UsageReported: a.usageReported}
}

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
