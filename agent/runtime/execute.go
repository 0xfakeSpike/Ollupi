package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/contextmanager"
	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

type callActionKind int

const (
	actionRun callActionKind = iota
	actionRunReadOnly
	actionSkip
	actionWait
	actionHalt
)

type callAction struct {
	kind   callActionKind
	item   repo.ItemRow
	job    *job
	reason string
}

type callPlan struct {
	actions []callAction
}

// executePlan runs a plan in order. Consecutive read-only actions execute in
// parallel; a serial action flushes the pending read-only batch and runs
// alone. A wait action ends the step with Wait, and a halt action finishes the
// turn after everything planned before it has completed.
func (rt *Runtime) executePlan(ctx context.Context, plan callPlan) (Outcome, bool, error) {
	var batch []job
	halt := ""
	flush := func() (string, error) {
		if len(batch) == 0 {
			return "", nil
		}
		var h string
		results := rt.runJobs(ctx, batch)
		for i, j := range batch {
			next, err := rt.finishTool(j, results[i])
			if err != nil {
				return h, err
			}
			if h == "" {
				h = next
			}
		}
		batch = batch[:0]
		return h, ctx.Err()
	}
	runOne := func(j job) (string, error) {
		results := rt.runJobs(ctx, []job{j})
		h, err := rt.finishTool(j, results[0])
		return h, errors.Join(err, ctx.Err())
	}
	for _, a := range plan.actions {
		if err := ctx.Err(); err != nil {
			return Done, true, err
		}
		switch a.kind {
		case actionRunReadOnly:
			if halt != "" {
				if err := rt.fail(a.item, "skipped: the turn stopped before this call was executed"); err != nil {
					return Done, true, err
				}
				continue
			}
			batch = append(batch, *a.job)
		case actionRun:
			h, err := flush()
			if err != nil {
				return Done, true, err
			}
			if halt == "" {
				halt = h
			}
			if halt != "" {
				if err := rt.fail(a.item, "skipped: the turn stopped before this call was executed"); err != nil {
					return Done, true, err
				}
				continue
			}
			h, err = runOne(*a.job)
			if err != nil {
				return Done, true, err
			}
			if halt == "" {
				halt = h
			}
		case actionSkip:
			if _, err := flush(); err != nil {
				return Done, true, err
			}
			if err := rt.fail(a.item, a.reason); err != nil {
				return Done, true, err
			}
		case actionWait:
			if _, err := flush(); err != nil {
				return Done, true, err
			}
			return Wait, true, nil
		case actionHalt:
			if _, err := flush(); err != nil {
				return Done, true, err
			}
			halt = a.reason
		}
	}
	h, err := flush()
	if err != nil {
		return Done, true, err
	}
	if halt == "" {
		halt = h
	}
	if rt.awaiting() {
		return Wait, true, nil
	}
	if halt != "" {
		return Done, true, rt.finish(ctx, halt)
	}
	return Continue, false, nil
}

// runJobs executes a batch in parallel, records their progress lines in order,
// and returns the results. It is also used with one job for serial calls, so
// progress and recording behave the same in both paths.
func (rt *Runtime) runJobs(ctx context.Context, jobs []job) []tools.Result {
	results := make([]tools.Result, len(jobs))
	progress := make(chan string, 64)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		emit := func(text string) {
			select {
			case progress <- text:
			case <-ctx.Done():
			}
		}
		go func() {
			defer wg.Done()
			results[i] = execute(ctx, rt.Registry, rt.Contexts, rt.Caller, j, emit)
		}()
	}
	go func() {
		wg.Wait()
		close(progress)
	}()
	for text := range progress {
		rt.recordProgress(text)
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
	if _, err := rt.record(repo.ItemRow{
		ID:          uuid.NewString(),
		TurnID:      rt.Turn.ID,
		Type:        repo.ItemToolProgress,
		Status:      repo.ItemCompleted,
		Text:        text,
		StartedAt:   now,
		CompletedAt: &now,
	}); err != nil {
		log.Printf("runtime: record tool progress: %v", err)
	}
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
