package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/contextmanager"
	"github.com/0xfakeSpike/ollupi/agent/permission"
	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

func (rt *Runtime) handleCalls(ctx context.Context) (string, *repo.ItemRow, error) {
	var halt string
	var batch []job
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}

		results := rt.executeBatch(ctx, batch)
		for i, j := range batch {
			h, err := rt.finishTool(j, results[i])
			if err != nil {
				return err
			}
			if halt == "" {
				halt = h
			}
		}
		batch = batch[:0]
		return ctx.Err()
	}
	for _, it := range rt.Items {
		if !it.IsCall() || it.Status != repo.ItemInProgress {
			continue
		}
		if halt != "" {
			if err := rt.fail(it, "skipped: the turn stopped before this call was executed"); err != nil {
				return "", nil, err
			}
			continue
		}
		h, pending, ready, err := rt.handleCall(ctx, it)
		if err != nil {
			return "", nil, err
		}
		switch {
		case ready != nil && rt.readOnly(ready.it.Tool):
			batch = append(batch, *ready)
			continue
		case ready != nil:
			if err := flush(); err != nil {
				return "", nil, err
			}
			if halt != "" {
				if err := rt.fail(ready.it, "skipped: the turn stopped before this call was executed"); err != nil {
					return "", nil, err
				}
				continue
			}
			batch = append(batch, *ready)
			if err := flush(); err != nil {
				return "", nil, err
			}
			continue
		}

		if err := flush(); err != nil {
			return "", nil, err
		}

		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		if pending != nil {
			return "", pending, nil
		}
		if halt == "" {
			halt = h
		}
	}
	if err := flush(); err != nil {
		return "", nil, err
	}
	return halt, nil, nil
}

type job struct {
	it   repo.ItemRow
	args map[string]any
}

func (rt *Runtime) readOnly(tool string) bool {
	d, ok := rt.tool(tool)
	return ok && d.Annotations.ReadOnlyHint && !d.Serial
}

func (rt *Runtime) handleCall(ctx context.Context, it repo.ItemRow) (string, *repo.ItemRow, *job, error) {
	if it.Await != nil {
		return "", nil, nil, nil
	}
	if it.Approval != nil {
		return rt.decided(it)
	}
	descriptor, offered := rt.tool(it.Tool)
	if !offered {
		h, err := rt.refuse(it, fmt.Sprintf("tool %q is not available in this turn; use an exact tool name from the tool list", it.Tool))
		return h, nil, nil, err
	}
	args, err := callArguments(it)
	if err != nil {
		h, err := rt.refuse(it, err.Error())
		return h, nil, nil, err
	}

	if err := tools.ValidateArguments(args, descriptor.InputSchema); err != nil {
		h, err := rt.refuse(it, fmt.Sprintf("invalid arguments for %s: %v", it.Tool, err))
		return h, nil, nil, err
	}

	if it.PreparedExecution == nil {
		prepared, err := rt.Registry.Prepare(ctx, it.Tool, args)
		if err != nil {
			h, err := rt.refuse(it, err.Error())
			return h, nil, nil, err
		}
		if prepared != nil {
			it.PreparedExecution = prepared
			stored, err := rt.record(it)
			if err != nil {
				return "", nil, nil, err
			}
			it = stored
		}
	}
	content, hasContent, err := rt.Registry.PermissionContent(it.Tool, args)
	if err != nil {
		h, err := rt.refuse(it, err.Error())
		return h, nil, nil, err
	}
	call := permission.Call{Tool: it.Tool, Annotations: *it.Annotations, Content: content, HasContent: hasContent, Prepared: it.PreparedExecution}
	decision := rt.Permissions.Decide(call, slices.Concat(rt.History, rt.Items))
	if decision.Verdict == permission.Deny {
		h, err := rt.refuse(it, decision.Reason)
		return h, nil, nil, err
	}
	if rt.toolCalls() >= maxToolCalls {
		if err := rt.fail(it, "skipped: the tool call limit for this turn was reached"); err != nil {
			return "", nil, nil, err
		}
		return fmt.Sprintf("reached the tool call limit (%d)", maxToolCalls), nil, nil, nil
	}
	if decision.Verdict == permission.Ask {
		it.Approval = decision.Approval(uuid.NewString(), call, time.Now().UTC())
		stored, err := rt.record(it)
		if err != nil {
			return "", nil, nil, err
		}
		return "", &stored, nil, nil
	}
	return "", nil, &job{it: it, args: args}, nil
}

func (rt *Runtime) decided(it repo.ItemRow) (string, *repo.ItemRow, *job, error) {
	switch it.Approval.Decision {
	case repo.DecisionAccept, repo.DecisionAcceptForSession:
		args, err := callArguments(it)
		if err != nil {
			return "", nil, nil, fmt.Errorf("runtime: approved call %s: %w", it.ID, err)
		}
		return "", nil, &job{it: it, args: args}, nil
	case repo.DecisionDecline:
		if _, err := rt.complete(withError(it, "the user declined this call"), repo.ItemDeclined); err != nil {
			return "", nil, nil, err
		}
		return fmt.Sprintf("the user declined %s", it.Tool), nil, nil, nil
	default:
		return "", nil, nil, fmt.Errorf("runtime: call %s has no approval decision", it.ID)
	}
}

func callArguments(it repo.ItemRow) (map[string]any, error) {
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

func (rt *Runtime) executeBatch(ctx context.Context, batch []job) []tools.Result {
	results := make([]tools.Result, len(batch))
	type line struct {
		call int
		text string
	}
	progress := make(chan line)
	done := make(chan struct{})
	for i, j := range batch {
		emit := func(text string) {
			select {
			case progress <- line{i, text}:
			case <-ctx.Done():
			}
		}
		go func() {
			results[i] = execute(ctx, rt.Registry, rt.Contexts, rt.Caller, j, emit)
			done <- struct{}{}
		}()
	}
	for running := len(batch); running > 0; {
		select {
		case l := <-progress:
			rt.recordProgress(l.text)
		case <-done:
			running--
		}
	}
	return results
}

func execute(ctx context.Context, registry *tools.Registry, contexts *contextmanager.Manager, caller tools.Caller, j job, emit tools.ProgressEmitter) tools.Result {
	it, args := j.it, j.args
	ctx = tools.WithCaller(tools.WithProgressEmitter(ctx, emit), caller)
	result := registry.InvokePrepared(ctx, it.Tool, args, it.PreparedExecution)
	if result.Err == nil {
		if err := checkArtifacts(result.Artifacts, contexts); err != nil {
			result = tools.Result{Err: fmt.Errorf("apply tool context: %w", err)}.For(it.Tool)
		}
	}
	return result
}

func (rt *Runtime) finishTool(j job, result tools.Result) (string, error) {
	it := j.it
	if result.Err == nil && result.Await != nil {
		it.Await = result.Await
		_, err := rt.record(it)
		return "", err
	}
	stored, errText, err := repo.ResultOf(result)
	if err != nil {
		stored, errText, err = repo.ResultOf(tools.Result{Err: fmt.Errorf("tool output cannot be recorded: %w", err)}.For(it.Tool))
		if err != nil {
			return "", err
		}
	}
	it.Result, it.Error = stored, errText
	status := repo.ItemCompleted
	if errText != "" {
		status = repo.ItemFailed
	}
	if _, err := rt.complete(it, status); err != nil {
		return "", err
	}
	if status == repo.ItemCompleted {
		return "", nil
	}
	if n := rt.consecutiveFailures(); n >= maxConsecutiveFailures {
		return fmt.Sprintf("%d consecutive failed tool calls; last error: %s", n, errText), nil
	}
	return "", nil
}

func (rt *Runtime) recordProgress(text string) {
	now := time.Now().UTC()
	_, _ = rt.record(repo.ItemRow{
		ID:          uuid.NewString(),
		TurnID:      rt.Turn.ID,
		Type:        repo.ItemToolProgress,
		Status:      repo.ItemCompleted,
		Text:        text,
		StartedAt:   now,
		CompletedAt: &now,
	})
}

func (rt *Runtime) fail(it repo.ItemRow, reason string) error {
	_, err := rt.complete(withError(it, reason), repo.ItemFailed)
	return err
}

func (rt *Runtime) refuse(it repo.ItemRow, reason string) (string, error) {
	if err := rt.fail(it, reason); err != nil {
		return "", err
	}
	if n := rt.consecutiveFailures(); n >= maxConsecutiveFailures {
		return fmt.Sprintf("%d consecutive invalid or failed tool calls; last: %s", n, reason), nil
	}
	return "", nil
}

const maxArtifactRunes = 20000

func checkArtifacts(artifacts []tools.ContextArtifact, contexts *contextmanager.Manager) error {
	for _, artifact := range artifacts {
		if artifact.Name == "" || strings.TrimSpace(artifact.Content) == "" {
			return fmt.Errorf("context artifact requires a name and content")
		}
		if artifact.Kind != tools.ArtifactAgentSkill {
			return fmt.Errorf("context artifact %s/%s has unsupported kind", artifact.Kind, artifact.Name)
		}
		if len([]rune(artifact.Content)) > maxArtifactRunes {
			return fmt.Errorf("context artifact %s/%s exceeds %d runes", artifact.Kind, artifact.Name, maxArtifactRunes)
		}
	}
	return contexts.CheckArtifacts(artifacts)
}

func (rt *Runtime) toolCalls() int {
	n := 0
	for _, it := range rt.Items {
		if it.IsCall() && (it.Result != nil || it.Approval != nil || it.Await != nil) {
			n++
		}
	}
	return n
}

func (rt *Runtime) consecutiveFailures() int {
	n := 0
	items := rt.Items
	for i := len(items) - 1; i >= 0; i-- {
		if !items[i].IsCall() || items[i].Status == repo.ItemInProgress {
			continue
		}
		if items[i].Status != repo.ItemFailed && items[i].Status != repo.ItemDeclined {
			break
		}
		n++
	}
	return n
}
