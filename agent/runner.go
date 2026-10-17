package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/contextmanager"
	"github.com/0xfakeSpike/ollupi/execution"
	"github.com/0xfakeSpike/ollupi/llm"
	"github.com/0xfakeSpike/ollupi/memory"
	"github.com/0xfakeSpike/ollupi/redact"
	"github.com/0xfakeSpike/ollupi/thread"
	"github.com/0xfakeSpike/ollupi/tools"
	"github.com/google/uuid"
)

const (
	maxToolCalls           = 16
	maxModelCalls          = 32
	maxConsecutiveFailures = 3
)

// Runner runs agents. It holds what every agent shares: the model client,
// the tool registry, the skills catalog, the context manager, and the
// legacy fallback instructions. Each turn supplies its own instruction snapshot.
type Runner struct {
	model        ChatModel
	tools        ToolRegistry
	skills       SkillCatalog
	contexts     *contextmanager.Manager
	memory       memory.Reader
	instructions string
}

// RunnerConfig are the Runner's collaborators.
type RunnerConfig struct {
	Model    ChatModel
	Tools    ToolRegistry
	Skills   SkillCatalog
	Contexts *contextmanager.Manager
	// Memory is optional. Memory maintenance remains outside the runtime;
	// the runner only reads a bounded selection once for each new turn.
	Memory memory.Reader
	// Instructions are used only by legacy callers without an execution snapshot.
	Instructions string
}

func NewRunner(c RunnerConfig) (*Runner, error) {
	if c.Model == nil || c.Tools == nil || c.Skills == nil || c.Contexts == nil {
		return nil, fmt.Errorf("agent runner: model, tools, skills, and context manager are required")
	}
	return &Runner{
		model:        c.Model,
		tools:        c.Tools,
		skills:       c.Skills,
		contexts:     c.Contexts,
		memory:       c.Memory,
		instructions: strings.TrimSpace(c.Instructions),
	}, nil
}

// Check validates an agent against the tool catalog, so a misconfigured
// agent fails at startup instead of on its first turn.
func (rn *Runner) Check(ctx context.Context, a Agent) error {
	_, err := rn.toolsFor(ctx, a)
	return err
}

// toolsFor returns the descriptors offered to agent a, in catalog order,
// followed by the runtime tools.
func (rn *Runner) toolsFor(ctx context.Context, a Agent) ([]tools.ToolDescriptor, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	rules, err := a.Permissions.parse()
	if err != nil {
		return nil, err
	}
	catalog, err := rn.tools.ToolDescriptors(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent runner: tool catalog: %w", err)
	}
	descriptors := catalog
	if a.Tools != nil {
		for _, name := range a.Tools {
			if !slices.ContainsFunc(catalog, func(d tools.ToolDescriptor) bool { return d.Name == name }) {
				return nil, fmt.Errorf("agent %s: tool %q is not in the tool registry", a.Name, name)
			}
		}
		descriptors = slices.DeleteFunc(slices.Clone(catalog), func(d tools.ToolDescriptor) bool {
			return !slices.Contains(a.Tools, d.Name)
		})
	}
	runtimeTools := runtimeToolDescriptors()
	if len(a.Subagents) > 0 {
		runtimeTools = append(runtimeTools, agentToolDescriptor(a.Subagents))
	}
	for _, r := range rules.all() {
		if slices.ContainsFunc(runtimeTools, func(d tools.ToolDescriptor) bool { return d.Name == r.tool }) {
			return nil, fmt.Errorf("agent %s: permission rule %q: %s never needs approval and takes no rules", a.Name, r.text, r.tool)
		}
		if !slices.ContainsFunc(catalog, func(d tools.ToolDescriptor) bool { return d.Name == r.tool }) {
			return nil, fmt.Errorf("agent %s: permission rule %q: tool %q is not in the tool registry", a.Name, r.text, r.tool)
		}
		if r.pattern != nil && !rn.tools.HasPermissionContent(r.tool) {
			return nil, fmt.Errorf("agent %s: permission rule %q: %s takes no specifier; write %q to match every call", a.Name, r.text, r.tool, r.tool)
		}
	}
	// A tool denied outright is not offered at all, as in Claude Code.
	descriptors = slices.DeleteFunc(slices.Clone(descriptors), func(d tools.ToolDescriptor) bool { return rules.removes(d.Name) })
	for _, runtimeTool := range runtimeTools {
		for _, descriptor := range catalog {
			if descriptor.Name == runtimeTool.Name {
				return nil, fmt.Errorf("agent runner: registry tool %q collides with a runtime tool", descriptor.Name)
			}
		}
		descriptors = append(descriptors, runtimeTool)
	}
	return descriptors, nil
}

// Turn is one turn to run: its id, every item of the thread so far, and
// the messages the user sends while it runs. A new turn ends with its user
// message; a resumed turn ends with the request the user just answered.
type Turn struct {
	ID               string
	Items            []thread.Item
	Input            PendingInput
	ExecutionContext *execution.Context
}

// Result is how Run left the turn: finished (its final answer is the last
// agentMessage item), paused on Pending, an item waiting for the user's
// approval or answers, or Waiting for the subagents its Agent calls launch.
// A paused turn is resumed by running it again once the item holds the
// response, or once every Agent call holds its subagent's report.
type Result struct {
	Pending *thread.Item
	Waiting bool
	// Usage is the token usage of this Run's model calls. It is also set
	// when Run returns an error.
	Usage thread.Usage
}

// run is the per-turn state. The tool list stays fixed across model calls so
// the request prefix (system message and tool list) is identical every call.
type run struct {
	agent            Agent
	turnID           string
	items            []thread.Item
	recorder         Recorder
	input            PendingInput
	rules            ruleSet
	tools            []llm.ToolSpec
	offered          map[string]tools.ToolDescriptor
	skills           []map[string]any
	usage            thread.Usage
	trace            []string
	executionContext *execution.Context
	memory           []thread.MemoryReference
	redactor         *redact.Redactor
}

// record persists an item, then applies the stored version to the run's
// copy of the thread.
func (r *run) record(it thread.Item) (thread.Item, error) {
	stored, err := r.recorder.Record(it)
	if err != nil {
		return thread.Item{}, fmt.Errorf("agent runner: record %s item: %w", it.Type, err)
	}
	for i := range r.items {
		if r.items[i].ID == stored.ID {
			r.items[i] = stored
			return stored, nil
		}
	}
	r.items = append(r.items, stored)
	return stored, nil
}

func (r *run) tracef(format string, args ...any) {
	r.trace = append(r.trace, fmt.Sprintf(format, args...))
}

// turnItems returns the items of the current turn.
func (r *run) turnItems() []thread.Item {
	var out []thread.Item
	for _, it := range r.items {
		if it.TurnID == r.turnID {
			out = append(out, it)
		}
	}
	return out
}

// modelCalls counts the model responses of the turn.
func (r *run) modelCalls() int {
	seen := map[string]bool{}
	for _, it := range r.turnItems() {
		if it.ResponseID != "" && it.Type != thread.ItemContextCompaction {
			seen[it.ResponseID] = true
		}
	}
	return len(seen)
}

// toolCalls counts the registry tool calls of the turn that were attempted
// (sent for approval or run), not refused, and the subagents launched.
func (r *run) toolCalls() int {
	n := 0
	for _, it := range r.turnItems() {
		if (it.Type == thread.ItemMcpToolCall && (it.Result != nil || it.Approval != nil)) || (it.Type == thread.ItemCollabAgentToolCall && it.Agent != "") {
			n++
		}
	}
	return n
}

// waitsOnAgents reports whether an Agent call of the turn still waits for
// its subagent.
func (r *run) waitsOnAgents() bool {
	return slices.ContainsFunc(r.turnItems(), thread.Item.WaitsOnAgent)
}

// consecutiveFailures counts the turn's trailing failed calls, ignoring
// calls not handled yet.
func (r *run) consecutiveFailures() int {
	n := 0
	items := r.turnItems()
	for i := len(items) - 1; i >= 0; i-- {
		if !items[i].IsCall() || items[i].Status == thread.ItemInProgress {
			continue
		}
		if items[i].Status != thread.ItemFailed && items[i].Status != thread.ItemDeclined {
			break
		}
		n++
	}
	return n
}

// hasFailedCall reports whether an identical registry call already failed
// or was declined in this turn.
func (r *run) hasFailedCall(fingerprint string) bool {
	for _, it := range r.turnItems() {
		if it.Type != thread.ItemMcpToolCall || (it.Status != thread.ItemFailed && it.Status != thread.ItemDeclined) || (it.Result == nil && it.Approval == nil) {
			continue
		}
		var args map[string]any
		if json.Unmarshal([]byte(it.Arguments), &args) == nil && thread.Fingerprint(it.Tool, args) == fingerprint {
			return true
		}
	}
	return false
}

// stop ends tool use; the turn then ends with a forced final answer.
type stop struct {
	reason string
}

// Run executes agent a on a new or resumed turn until it finishes or pauses
// for the user. An error means the turn could not be completed or recorded;
// the caller records the failure.
func (rn *Runner) Run(ctx context.Context, a Agent, turn Turn, recorder Recorder) (Result, error) {
	r, err := rn.start(ctx, a, turn, recorder)
	if err != nil {
		return Result{}, err
	}
	defer func() { log.Printf("agent %s: turn %s\n%s", a.Name, turn.ID, strings.Join(r.trace, "\n")) }()
	result, err := rn.loop(ctx, r)
	result.Usage = r.usage
	return result, err
}

func (rn *Runner) start(ctx context.Context, a Agent, turn Turn, recorder Recorder) (*run, error) {
	if turn.Input == nil {
		return nil, fmt.Errorf("agent runner: turn %s has no pending input", turn.ID)
	}
	for _, it := range turn.Items {
		if it.IsPending() {
			return nil, fmt.Errorf("agent runner: item %s still waits for the user", it.ID)
		}
		if it.WaitsOnAgent() {
			return nil, fmt.Errorf("agent runner: item %s still waits for its subagent", it.ID)
		}
	}
	descriptors, err := rn.toolsFor(ctx, a)
	if err != nil {
		return nil, err
	}
	rules, err := a.Permissions.parse()
	if err != nil {
		return nil, err
	}
	// Every tool is offered on every call, in catalog order, so the tool
	// list is byte-identical across the turn and the prompt prefix caches.
	r := &run{
		agent:            a,
		turnID:           turn.ID,
		items:            turn.Items,
		recorder:         recorder,
		input:            turn.Input,
		rules:            rules,
		tools:            make([]llm.ToolSpec, 0, len(descriptors)),
		offered:          make(map[string]tools.ToolDescriptor, len(descriptors)),
		skills:           rn.skills.Descriptors(),
		executionContext: execution.Clone(turn.ExecutionContext),
	}
	for _, descriptor := range descriptors {
		r.tools = append(r.tools, llm.ToolSpec{Name: descriptor.Name, Description: strings.TrimSpace(descriptor.Description), Parameters: descriptor.InputSchema})
		r.offered[descriptor.Name] = descriptor
	}
	r.tracef("model=%s tools offered=%d approval_policy=%s", a.Model, len(descriptors), a.ApprovalPolicy)
	r.redactor = redact.New()
	// Rebuild the reverse map from the persisted items so a resumed turn can
	// resolve placeholders it produced before pausing.
	for _, it := range turn.Items {
		r.redactor.Collect(it.Text)
		r.redactor.Collect(it.Error)
		r.redactor.Collect(it.Arguments)
		if it.Result != nil {
			r.redactor.Collect(string(it.Result.Output))
			for _, artifact := range it.Result.Artifacts {
				r.redactor.Collect(artifact.Content)
			}
		}
		if it.Answers != nil {
			if raw, err := json.Marshal(it.Answers); err == nil {
				r.redactor.Collect(string(raw))
			}
		}
	}
	if err := rn.loadMemory(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

func (rn *Runner) loop(ctx context.Context, r *run) (Result, error) {
	for {
		// Calls of the last response not handled yet: after a model
		// response, or when the turn resumes after the user responded.
		halt, pending, err := rn.handleCalls(ctx, r)
		if err != nil {
			return Result{}, err
		}
		if pending != nil {
			r.tracef("turn paused on %s %s", pending.Type, pending.Tool)
			return Result{Pending: pending}, nil
		}
		if r.waitsOnAgents() {
			r.tracef("turn waits for subagents")
			return Result{Waiting: true}, nil
		}
		// What the user sent while the calls ran comes before the next
		// response.
		if _, err := rn.takeInput(r, false); err != nil {
			return Result{}, err
		}
		if halt != nil {
			return rn.finish(ctx, r, *halt)
		}
		if r.modelCalls() >= maxModelCalls {
			return rn.finish(ctx, r, stop{reason: fmt.Sprintf("reached the model call limit (%d)", maxModelCalls)})
		}
		resp, _, err := rn.respond(ctx, r, llm.ToolChoiceAuto)
		if err != nil {
			return Result{}, err
		}
		if len(resp.Message.ToolCalls) == 0 {
			// A message sent while the model answered continues the turn.
			more, err := rn.takeInput(r, true)
			if err != nil {
				return Result{}, err
			}
			if more {
				continue
			}
			r.tracef("turn answered")
			return Result{}, nil
		}
	}
}

// handleCalls handles the turn's call items still in progress, in order. It
// returns a stop when tool use must end, or the item the turn now waits on.
func (rn *Runner) handleCalls(ctx context.Context, r *run) (*stop, *thread.Item, error) {
	var halt *stop
	for _, it := range r.turnItems() {
		if !it.IsCall() || it.Status != thread.ItemInProgress {
			continue
		}
		if halt != nil {
			if err := rn.fail(r, it, "skipped: the turn stopped before this call was executed"); err != nil {
				return nil, nil, err
			}
			continue
		}
		h, pending, err := rn.handleCall(ctx, r, it)
		if err != nil {
			return nil, nil, err
		}
		// An interrupted turn ends now; its unfinished items are closed
		// when the turn is recorded as interrupted.
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if pending != nil {
			return nil, pending, nil
		}
		halt = h
	}
	return halt, nil, nil
}

// streamedText records the reasoning and agentMessage items of one model
// response as they stream: an item starts on its first delta, receives each
// delta, and completes with the whole text when the response ends.
type streamedText struct {
	r          *run
	responseID string
	items      map[thread.ItemType]*thread.Item
	stats      *thread.ContextStats
	usage      *thread.Usage
	carrierID  string
	// text is what has streamed into each item so far.
	text map[thread.ItemType]*strings.Builder
	err  error
}

func newStreamedText(r *run, stats contextmanager.Stats) *streamedText {
	return &streamedText{r: r, responseID: uuid.NewString(), items: map[thread.ItemType]*thread.Item{}, text: map[thread.ItemType]*strings.Builder{}, stats: contextStats(stats)}
}

// record puts the request snapshot and usage on exactly one item: the first
// item successfully persisted for this response. Later text or tool updates
// keep the same carrier, even when the provider streams content before reasoning.
func (st *streamedText) record(it thread.Item) (thread.Item, error) {
	if st.carrierID == "" || st.carrierID == it.ID {
		it.ContextStats, it.Usage = st.stats, st.usage
	}
	stored, err := st.r.record(it)
	if err == nil && st.carrierID == "" {
		st.carrierID = stored.ID
	}
	return stored, err
}

func (st *streamedText) delta(d llm.Delta) {
	st.add(thread.ItemReasoning, d.Reasoning)
	st.add(thread.ItemAgentMessage, d.Content)
}

func (st *streamedText) add(kind thread.ItemType, text string) {
	if text == "" || st.err != nil {
		return
	}
	it, ok := st.items[kind]
	if !ok {
		started, err := st.record(thread.Item{
			ID: uuid.NewString(), TurnID: st.r.turnID, Type: kind, Status: thread.ItemInProgress,
			ResponseID: st.responseID, StartedAt: time.Now().UTC(),
		})
		if err != nil {
			// Recording failed; the response is discarded once it ends.
			st.err = err
			return
		}
		it = &started
		st.items[kind] = it
		st.text[kind] = &strings.Builder{}
	}
	st.text[kind].WriteString(text)
	st.r.recorder.Delta(*it, text)
}

// complete records the whole text of each streamed item. Text the stream
// did not deliver as deltas (a provider that sends it only at the end) is
// recorded here too, so the items always match the response.
func (st *streamedText) complete(resp llm.ChatResponse) error {
	if st.err != nil {
		return st.err
	}
	for _, part := range []struct {
		kind thread.ItemType
		text string
	}{{thread.ItemReasoning, resp.Message.ReasoningContent}, {thread.ItemAgentMessage, resp.Message.Content}} {
		it, started := st.items[part.kind]
		if part.text == "" && !started {
			continue
		}
		now := time.Now().UTC()
		if !started {
			it = &thread.Item{ID: uuid.NewString(), TurnID: st.r.turnID, Type: part.kind, ResponseID: st.responseID, StartedAt: now}
		}
		it.Text, it.Status, it.CompletedAt = part.text, thread.ItemCompleted, &now
		if _, err := st.record(*it); err != nil {
			return err
		}
	}
	return nil
}

// fail closes the items of a response that ended in an error, keeping the
// text that already streamed. Failed text is never shown to the model.
func (st *streamedText) fail(cause error) error {
	if len(st.items) == 0 {
		now := time.Now().UTC()
		if _, err := st.record(thread.Item{
			ID: uuid.NewString(), TurnID: st.r.turnID, Type: thread.ItemError, Status: thread.ItemFailed,
			ResponseID: st.responseID, Text: cause.Error(), Error: cause.Error(), StartedAt: now, CompletedAt: &now,
		}); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}
	for kind, it := range st.items {
		now := time.Now().UTC()
		it.Text, it.Status, it.Error, it.CompletedAt = st.text[kind].String(), thread.ItemFailed, cause.Error(), &now
		if _, err := st.record(*it); err != nil {
			return errors.Join(cause, err)
		}
	}
	return cause
}

// callItem is the in-progress item for one model tool call.
func callItem(r *run, responseID string, call llm.ToolCall) thread.Item {
	it := thread.Item{
		ID: uuid.NewString(), TurnID: r.turnID, Type: thread.ItemMcpToolCall, Status: thread.ItemInProgress,
		ResponseID: responseID, CallID: call.ID, Tool: call.Name, Arguments: call.Arguments, StartedAt: time.Now().UTC(),
	}
	switch call.Name {
	case ToolUpdatePlan:
		it.Type = thread.ItemTodoList
	case ToolRequestUserInput:
		it.Type = thread.ItemUserInputRequest
	case ToolAgent:
		it.Type = thread.ItemCollabAgentToolCall
	default:
		if d, ok := r.offered[call.Name]; ok {
			it.Server = d.Provider
			annotations := d.Annotations
			it.Annotations = &annotations
		}
	}
	return it
}

func (rn *Runner) contextInput(r *run) contextmanager.Input {
	in := contextmanager.Input{
		Items:               r.items,
		CurrentTurnID:       r.turnID,
		Memory:              r.memory,
		Rules:               r.agent.Instructions + "\n\n" + systemRules,
		ProjectInstructions: rn.instructions,
		Skills:              r.skills,
		Tools:               r.tools,
	}
	if r.executionContext != nil {
		in.ProjectInstructions = ""
		in.ExecutionContext = r.executionContext
	}
	return in
}

// respond makes one model call of the turn, compacting the thread first
// when its context has filled up. The response's text is streamed into
// reasoning and agentMessage items. All call items are recorded before any
// tool executes, so a paused turn still knows every requested call.
func (rn *Runner) respond(ctx context.Context, r *run, choice llm.ToolChoice) (llm.ChatResponse, string, error) {
	prompt, err := rn.contexts.Build(rn.contextInput(r))
	if err != nil {
		return llm.ChatResponse{}, "", fmt.Errorf("agent runner: %w", err)
	}
	if prompt.NeedsCompaction {
		if err := rn.compact(ctx, r); err != nil {
			return llm.ChatResponse{}, "", err
		}
		if prompt, err = rn.contexts.Build(rn.contextInput(r)); err != nil {
			return llm.ChatResponse{}, "", fmt.Errorf("agent runner: after compaction: %w", err)
		}
	}
	call := r.modelCalls() + 1
	st := newStreamedText(r, prompt.Stats)
	resp, err := rn.send(ctx, r, prompt, choice, st.delta)
	st.usage = responseUsage(resp)
	if err == nil {
		err = rn.check(resp, choice)
	}
	if err != nil {
		return llm.ChatResponse{}, "", st.fail(fmt.Errorf("agent runner: model call %d: %w", call, err))
	}
	if err := st.complete(resp); err != nil {
		return llm.ChatResponse{}, "", err
	}
	for _, call := range resp.Message.ToolCalls {
		if _, err := st.record(callItem(r, st.responseID, call)); err != nil {
			return llm.ChatResponse{}, "", err
		}
	}
	r.tracef("model call=%d %s prompt_tokens=%d cached_tokens=%d completion_tokens=%d finish=%s tool_calls=%d",
		call, prompt.Stats.Trace(), resp.Usage.PromptTokens, resp.Usage.CachedPromptTokens, resp.Usage.CompletionTokens, resp.FinishReason, len(resp.Message.ToolCalls))
	return resp, st.responseID, nil
}

// check rejects a response that cannot be recorded as given: tool calls
// when tools were withheld, or a final answer that is empty or truncated.
func (rn *Runner) check(resp llm.ChatResponse, choice llm.ToolChoice) error {
	if len(resp.Message.ToolCalls) > 0 {
		if choice == llm.ToolChoiceNone {
			return fmt.Errorf("model requested tools after tool use was stopped")
		}
		return nil
	}
	if resp.FinishReason == "length" {
		return fmt.Errorf("final answer truncated by the model output limit")
	}
	if strings.TrimSpace(resp.Message.Content) == "" {
		return fmt.Errorf("model returned neither a tool call nor an answer (finish_reason=%s)", resp.FinishReason)
	}
	return nil
}

// compact asks the model to summarize the thread and records the summary as
// a contextCompaction item, which replaces every earlier item in the
// prompt from then on.
func (rn *Runner) compact(ctx context.Context, r *run) error {
	prompt, err := rn.contexts.CompactionPrompt(rn.contextInput(r))
	if err != nil {
		return fmt.Errorf("agent runner: %w", err)
	}
	it, err := r.record(thread.Item{
		ID: uuid.NewString(), TurnID: r.turnID, Type: thread.ItemContextCompaction, Status: thread.ItemInProgress, StartedAt: time.Now().UTC(),
		ResponseID: uuid.NewString(), ContextStats: contextStats(prompt.Stats),
	})
	if err != nil {
		return err
	}
	resp, err := rn.send(ctx, r, prompt, llm.ToolChoiceNone, nil)
	it.Usage = responseUsage(resp)
	summary := strings.TrimSpace(resp.Message.Content)
	switch {
	case err != nil:
	case len(resp.Message.ToolCalls) > 0 || summary == "":
		err = fmt.Errorf("the model returned no summary (finish_reason=%s)", resp.FinishReason)
	case resp.FinishReason == "length":
		err = fmt.Errorf("the summary was truncated by the model output limit")
	}
	if err != nil {
		if _, rerr := rn.complete(r, withError(it, err.Error()), thread.ItemFailed); rerr != nil {
			return rerr
		}
		return fmt.Errorf("agent runner: compaction: %w", err)
	}
	it.Text = summary
	if _, err := rn.complete(r, it, thread.ItemCompleted); err != nil {
		return err
	}
	r.tracef("compacted %s; summary_tokens=%d", prompt.Stats.Trace(), resp.Usage.CompletionTokens)
	return nil
}

// send makes one model request with the turn's tool list and accounts its
// usage.
func (rn *Runner) send(ctx context.Context, r *run, prompt contextmanager.Prompt, choice llm.ToolChoice, onDelta func(llm.Delta)) (llm.ChatResponse, error) {
	messages := make([]llm.ChatMessage, 0, len(prompt.Messages)+1)
	messages = append(messages, llm.ChatMessage{Role: llm.RoleSystem, Content: r.redactor.Mask(prompt.System)})
	for _, message := range prompt.Messages {
		converted := llm.ChatMessage{Role: llm.Role(message.Role), Content: r.redactor.Mask(message.Content), ReasoningContent: r.redactor.Mask(message.Reasoning), ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, llm.ToolCall{ID: call.ID, Name: call.Name, Arguments: r.redactor.Mask(call.Arguments)})
		}
		messages = append(messages, converted)
	}
	resp, err := rn.model.Chat(ctx, llm.ChatRequest{
		Model: r.agent.Model, ModelProfileID: r.agent.ModelProfileID, EnableThinking: r.agent.ModelSettings.EnableThinking,
		Messages: messages, Tools: r.tools, ToolChoice: choice,
		// Several Agent calls in one response run in parallel.
		ParallelToolCalls: len(r.agent.Subagents) > 0,
	}, onDelta)
	if usage := responseUsage(resp); usage != nil {
		r.usage.Add(*usage)
	}
	return resp, err
}

func contextStats(stats contextmanager.Stats) *thread.ContextStats {
	return &thread.ContextStats{
		BudgetTokens: stats.BudgetTokens, SystemTokens: stats.SystemTokens,
		MessagesTokens: stats.MessagesTokens, ToolsTokens: stats.ToolsTokens, EstimatedTokens: stats.EstimatedTokens,
		MemoryTokens: stats.MemoryTokens,
	}
}

// Keep absent usage distinct from reported zero tokens, including when a
// provider supplies usage before a partial response ends in an error.
func responseUsage(resp llm.ChatResponse) *thread.Usage {
	if !resp.UsageReported {
		return nil
	}
	return &thread.Usage{InputTokens: resp.Usage.PromptTokens, CachedInputTokens: resp.Usage.CachedPromptTokens, OutputTokens: resp.Usage.CompletionTokens}
}

// handleCall handles one call item in progress: a new call, or one whose
// approval the user decided. It returns a stop when tool use must end, or
// the item when the turn must wait for the user.
func (rn *Runner) handleCall(ctx context.Context, r *run, it thread.Item) (*stop, *thread.Item, error) {
	if it.Approval != nil {
		return rn.decided(ctx, r, it)
	}
	descriptor, offered := r.offered[it.Tool]
	if !offered {
		h, err := rn.refuse(r, it, fmt.Sprintf("tool %q is not available in this turn; use an exact tool name from the tool list", it.Tool))
		return h, nil, err
	}
	args, err := callArguments(it)
	if err != nil {
		h, err := rn.refuse(r, it, err.Error())
		return h, nil, err
	}

	switch it.Type {
	case thread.ItemTodoList:
		plan, err := parsePlan(args)
		if err != nil {
			h, err := rn.refuse(r, it, fmt.Sprintf("invalid plan: %v", err))
			return h, nil, err
		}
		it.Plan = &plan
		_, err = rn.complete(r, it, thread.ItemCompleted)
		return nil, nil, err

	case thread.ItemUserInputRequest:
		questions, err := parseQuestions(args)
		if err != nil {
			h, err := rn.refuse(r, it, fmt.Sprintf("invalid questions: %v", err))
			return h, nil, err
		}
		// The item stays in progress until the user's answers fill it.
		it.Questions = questions
		stored, err := r.record(it)
		if err != nil {
			return nil, nil, err
		}
		return nil, &stored, nil

	case thread.ItemCollabAgentToolCall:
		if it.Agent != "" {
			// Launched earlier; it waits for its subagent's report.
			return nil, nil, nil
		}
		call, err := ParseAgentCall(it.Arguments, r.agent.Subagents)
		if err != nil {
			h, err := rn.refuse(r, it, fmt.Sprintf("invalid Agent call: %v", err))
			return h, nil, err
		}
		if r.toolCalls() >= maxToolCalls {
			if err := rn.fail(r, it, "skipped: the tool call limit for this turn was reached"); err != nil {
				return nil, nil, err
			}
			return &stop{reason: fmt.Sprintf("reached the tool call limit (%d)", maxToolCalls)}, nil, nil
		}
		// Accepted: the item stays in progress; once the turn pauses, the
		// thread manager launches the subagent and fills in its report.
		it.Agent = call.SubagentType
		_, err = r.record(it)
		r.tracef("subagent %s launched: %s", call.SubagentType, call.Description)
		return nil, nil, err
	}

	if err := tools.ValidateArguments(args, descriptor.InputSchema); err != nil {
		h, err := rn.refuse(r, it, fmt.Sprintf("invalid arguments for %s: %v", it.Tool, err))
		return h, nil, err
	}
	if r.hasFailedCall(thread.Fingerprint(it.Tool, args)) {
		h, err := rn.refuse(r, it, "refused: an identical call already failed in this turn; change the arguments or the strategy, or answer with the limitation")
		return h, nil, err
	}
	// Freeze local directories and normalized commands before the first approval
	// or execution. Resumed calls already have a persisted record and must use it.
	if r.executionContext != nil && it.PreparedExecution == nil {
		if registry, ok := rn.tools.(ExecutionToolRegistry); ok {
			prepared, err := registry.Prepare(ctx, it.Tool, args, r.executionContext)
			if err != nil {
				h, err := rn.refuse(r, it, err.Error())
				return h, nil, err
			}
			if prepared != nil {
				it.PreparedExecution = prepared
				stored, err := r.record(it)
				if err != nil {
					return nil, nil, err
				}
				it = stored
			}
		}
	}
	if prepared := it.PreparedExecution; prepared != nil && prepared.Escalated && !r.agent.ApprovalPolicy.AllowsEscalation() {
		h, err := rn.refuse(r, it, fmt.Sprintf("refused: under the approval policy %q nobody can approve running outside the sandbox; do the work inside the sandbox, or tell the user what needs to run outside it", r.agent.ApprovalPolicy))
		return h, nil, err
	}
	content, hasContent, err := rn.tools.PermissionContent(it.Tool, args)
	if err != nil {
		h, err := rn.refuse(r, it, err.Error())
		return h, nil, err
	}
	verdict, ruleText := r.rules.decide(it.Tool, content, hasContent)
	if verdict == permissionDeny {
		h, err := rn.refuse(r, it, fmt.Sprintf("denied by the permission rule %s; do not try it again, find another way or tell the user", ruleText))
		return h, nil, err
	}
	if r.toolCalls() >= maxToolCalls {
		if err := rn.fail(r, it, "skipped: the tool call limit for this turn was reached"); err != nil {
			return nil, nil, err
		}
		return &stop{reason: fmt.Sprintf("reached the tool call limit (%d)", maxToolCalls)}, nil, nil
	}
	approval := &thread.Approval{ID: uuid.NewString(), RequestedAt: time.Now().UTC()}
	switch {
	case verdict == permissionAsk:
		approval.Reason, approval.Rule = fmt.Sprintf("Your permission rule %s asks before this call.", ruleText), ruleText
	case verdict == permissionAllow:
		r.tracef("tool=%s allowed by the permission rule %s", it.Tool, ruleText)
		approval = nil
	default:
		granted, err := rn.grantedForSession(r, it.Tool, content, hasContent, it.PreparedExecution)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case granted:
			r.tracef("tool=%s allowed for this thread earlier", it.Tool)
			approval = nil
		case r.agent.ApprovalPolicy.RequiresApproval(*it.Annotations, it.PreparedExecution):
			// The policy is the usual reason; like Codex, no reason is given,
			// except the model's own for leaving the sandbox.
			if p := it.PreparedExecution; p != nil && p.Escalated {
				approval.Reason = "Runs outside the sandbox: " + p.Justification
			}
		default:
			approval = nil
		}
	}
	if approval != nil {
		// The turn waits for the user's decision; the item records the
		// request, so the wait survives a restart.
		it.Approval = approval
		stored, err := r.record(it)
		if err != nil {
			return nil, nil, err
		}
		return nil, &stored, nil
	}
	h, err := rn.runTool(ctx, r, it, args)
	return h, nil, err
}

// grantedForSession reports whether the user allowed this call earlier in
// the thread with acceptForSession (Codex): the same command, for a tool
// that takes a specifier, or else any call of the tool.
func (rn *Runner) grantedForSession(r *run, tool, content string, hasContent bool, prepared *execution.Prepared) (bool, error) {
	for _, it := range r.items {
		if it.Type != thread.ItemMcpToolCall || it.Tool != tool || it.Approval == nil || it.Approval.Decision != thread.DecisionAcceptForSession {
			continue
		}
		// Directory, execution context, normalized argv and environment all
		// belong to the approval; a command in another directory asks again.
		if execution.Digest(it.PreparedExecution) != execution.Digest(prepared) {
			continue
		}
		if !hasContent {
			return true, nil
		}
		args, err := callArguments(it)
		if err != nil {
			return false, fmt.Errorf("agent runner: approved call %s: %w", it.ID, err)
		}
		earlier, _, err := rn.tools.PermissionContent(tool, args)
		if err != nil {
			return false, fmt.Errorf("agent runner: approved call %s: %w", it.ID, err)
		}
		if earlier == content {
			return true, nil
		}
	}
	return false, nil
}

// decided continues a call whose approval the user decided: an accepted
// call runs, a declined one ends tool use.
func (rn *Runner) decided(ctx context.Context, r *run, it thread.Item) (*stop, *thread.Item, error) {
	switch it.Approval.Decision {
	case thread.DecisionAccept, thread.DecisionAcceptForSession:
		args, err := callArguments(it)
		if err != nil {
			return nil, nil, fmt.Errorf("agent runner: approved call %s: %w", it.ID, err)
		}
		h, err := rn.runTool(ctx, r, it, args)
		return h, nil, err
	case thread.DecisionDecline:
		if _, err := rn.complete(r, withError(it, "the user declined this call"), thread.ItemDeclined); err != nil {
			return nil, nil, err
		}
		r.tracef("tool=%s declined", it.Tool)
		return &stop{reason: fmt.Sprintf("the user declined %s", it.Tool)}, nil, nil
	default:
		return nil, nil, fmt.Errorf("agent runner: call %s has no approval decision", it.ID)
	}
}

// callArguments decodes a call's raw arguments. An empty string is the
// protocol's encoding of "no arguments".
func callArguments(it thread.Item) (map[string]any, error) {
	args := map[string]any{}
	if strings.TrimSpace(it.Arguments) == "" {
		return args, nil
	}
	if err := json.Unmarshal([]byte(it.Arguments), &args); err != nil {
		return nil, fmt.Errorf("arguments are not a valid JSON object: %v", err)
	}
	if args == nil {
		return nil, fmt.Errorf("arguments must be a JSON object, not null")
	}
	return args, nil
}

// subdirectoryInstructions returns the AGENTS.md files that apply in cwd
// and that the thread has not loaded yet in their current version. They
// are loaded once per thread: earlier turns' results keep them in the
// transcript, and compaction keeps them as active artifacts.
func subdirectoryInstructions(r *run, cwd string) ([]tools.ContextArtifact, error) {
	found, err := execution.SubdirectoryInstructions(r.executionContext, cwd)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	loaded := map[string]bool{}
	for _, it := range r.items {
		if it.Type != thread.ItemMcpToolCall || it.Status != thread.ItemCompleted || it.Result == nil {
			continue
		}
		for _, a := range it.Result.Artifacts {
			if a.Kind == tools.ArtifactInstructions {
				loaded[a.Name+"\x00"+a.Digest] = true
			}
		}
	}
	var out []tools.ContextArtifact
	for _, f := range found {
		if loaded[f.Path+"\x00"+f.Digest] {
			continue
		}
		out = append(out, tools.ContextArtifact{Kind: tools.ArtifactInstructions, Name: f.Path, Content: f.Content, Source: f.Path, Digest: f.Digest})
	}
	if len(out) > 0 {
		r.tracef("loaded %d subdirectory instruction file(s) for %s", len(out), cwd)
	}
	return out, nil
}

// runTool executes one registry tool call and completes its item.
func (rn *Runner) runTool(ctx context.Context, r *run, it thread.Item, args map[string]any) (*stop, error) {
	ctx = tools.WithProgressEmitter(ctx, rn.progressEmitter(r))
	if d, ok := r.offered[it.Tool]; ok && d.Redact == tools.RedactRestore {
		restoreArgs(r.redactor, args)
	}
	var result tools.Result
	// A command's first run in a project subdirectory brings that
	// directory's AGENTS.md files along with its result. They are read
	// before the command runs, so a broken file stops the call instead of
	// hiding the output of a command that already ran.
	var instructions []tools.ContextArtifact
	var instructionsErr error
	if r.executionContext != nil && it.PreparedExecution != nil {
		instructions, instructionsErr = subdirectoryInstructions(r, it.PreparedExecution.Cwd)
	}
	if instructionsErr != nil {
		result = tools.Result{Err: fmt.Errorf("not run: this directory's instructions cannot be loaded; ask the user to fix the file: %w", instructionsErr)}
	} else if r.executionContext != nil {
		if registry, ok := rn.tools.(ExecutionToolRegistry); ok {
			result = registry.InvokePrepared(ctx, it.Tool, args, r.executionContext, it.PreparedExecution)
			if result.Err == nil {
				result.Artifacts = append(result.Artifacts, instructions...)
			}
		} else {
			result = tools.Result{Err: fmt.Errorf("tool registry does not support this turn's execution context")}
		}
	} else if it.PreparedExecution != nil {
		result = tools.Result{Err: fmt.Errorf("prepared tool execution is missing its saved turn context")}
	} else {
		result = rn.tools.Invoke(ctx, it.Tool, args)
	}
	if result.Err == nil {
		// Instructions the context cannot hold are a tool failure,
		// decided before anything is recorded.
		if err := thread.CheckArtifacts(result.Artifacts); err != nil {
			result = tools.Result{Err: fmt.Errorf("apply tool context: %w", err)}.WithInferredMeta(it.Tool)
		}
	}
	stored, errText, err := thread.ResultOf(result)
	if err != nil {
		// Output that cannot be stored is a failure of this tool call.
		stored, errText, err = thread.ResultOf(tools.Result{Err: fmt.Errorf("tool output cannot be recorded: %w", err)}.WithInferredMeta(it.Tool))
		if err != nil {
			return nil, err
		}
	}
	it.Result, it.Error = stored, errText
	status := thread.ItemCompleted
	if errText != "" {
		status = thread.ItemFailed
	}
	if _, err := rn.complete(r, it, status); err != nil {
		return nil, err
	}
	r.tracef("tool=%s status=%s error=%s", it.Tool, status, errText)
	if status == thread.ItemCompleted {
		return nil, nil
	}
	if n := r.consecutiveFailures(); n >= maxConsecutiveFailures {
		return &stop{reason: fmt.Sprintf("%d consecutive failed tool calls; last error: %s", n, errText)}, nil
	}
	return nil, nil
}

// restoreArgs resolves redaction placeholders inside decoded tool arguments,
// in place. It is only called for tools that opt in with RedactRestore.
func restoreArgs(r *redact.Redactor, args map[string]any) {
	for k, v := range args {
		args[k] = restoreRedactedValue(r, v)
	}
}

func restoreRedactedValue(r *redact.Redactor, v any) any {
	switch x := v.(type) {
	case string:
		return r.Restore(x)
	case map[string]any:
		restoreArgs(r, x)
		return x
	case []any:
		for i := range x {
			x[i] = restoreRedactedValue(r, x[i])
		}
		return x
	default:
		return v
	}
}

// progressEmitter records a completed toolProgress item for each line a tool
// streams while it runs, so long-running tools can narrate into the feed.
func (rn *Runner) progressEmitter(r *run) tools.ProgressEmitter {
	return func(text string) {
		now := time.Now().UTC()
		_, _ = r.record(thread.Item{
			ID:          uuid.NewString(),
			TurnID:      r.turnID,
			Type:        thread.ItemToolProgress,
			Status:      thread.ItemCompleted,
			Text:        text,
			StartedAt:   now,
			CompletedAt: &now,
		})
	}
}

func withError(it thread.Item, text string) thread.Item {
	it.Error = text
	return it
}

// complete records it with a final status.
func (rn *Runner) complete(r *run, it thread.Item, status thread.ItemStatus) (thread.Item, error) {
	now := time.Now().UTC()
	it.Status = status
	it.CompletedAt = &now
	return r.record(it)
}

// fail records a call that was not executed.
func (rn *Runner) fail(r *run, it thread.Item, reason string) error {
	_, err := rn.complete(r, withError(it, reason), thread.ItemFailed)
	return err
}

// refuse answers a call the runtime refused to execute. Refusals count as
// failures so a model that keeps producing invalid calls is stopped.
func (rn *Runner) refuse(r *run, it thread.Item, reason string) (*stop, error) {
	if err := rn.fail(r, it, reason); err != nil {
		return nil, err
	}
	r.tracef("tool call refused tool=%s reason=%s", it.Tool, reason)
	if n := r.consecutiveFailures(); n >= maxConsecutiveFailures {
		return &stop{reason: fmt.Sprintf("%d consecutive invalid or failed tool calls; last: %s", n, reason)}, nil
	}
	return nil, nil
}

// finish forces a final answer without tools after the turn stopped tool
// use. The notice to the model is recorded as an error item.
func (rn *Runner) finish(ctx context.Context, r *run, why stop) (Result, error) {
	now := time.Now().UTC()
	if _, err := r.record(thread.Item{
		ID: uuid.NewString(), TurnID: r.turnID, Type: thread.ItemError, Status: thread.ItemCompleted,
		Text: fmt.Sprintf(finalAnswerNotice, why.reason), StartedAt: now, CompletedAt: &now,
	}); err != nil {
		return Result{}, err
	}
	for {
		if _, _, err := rn.respond(ctx, r, llm.ToolChoiceNone); err != nil {
			return Result{}, fmt.Errorf("%w (tool use stopped: %s)", err, why.reason)
		}
		// Tool use stays stopped; a message sent meanwhile gets an answer
		// without tools.
		more, err := rn.takeInput(r, true)
		if err != nil {
			return Result{}, err
		}
		if !more {
			break
		}
	}
	r.tracef("turn answered after stop: %s", why.reason)
	return Result{}, nil
}

// takeInput records the messages the user sent to the turn while it ran.
// With ending set, the input is closed when none wait (see
// PendingInput.TakeOrClose). It reports whether any message was recorded.
func (rn *Runner) takeInput(r *run, ending bool) (bool, error) {
	texts := r.input.Take
	if ending {
		texts = r.input.TakeOrClose
	}
	taken := texts()
	for _, text := range taken {
		if _, err := r.record(thread.NewUserMessage(uuid.NewString(), r.turnID, text, time.Now().UTC())); err != nil {
			return false, err
		}
	}
	if len(taken) > 0 {
		r.tracef("took %d message(s) sent during the turn", len(taken))
	}
	return len(taken) > 0, nil
}
