package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/permission"
	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

type job struct {
	it   repo.ItemRow
	args map[string]any
}

// planCalls walks the in-progress call items once and turns each into an
// action, without executing anything. It keeps the original semantics: an
// approval wait stops later calls, and once a call decided the turn must stop,
// the remaining calls are skipped.
func (rt *Runtime) planCalls(ctx context.Context) (callPlan, error) {
	var plan callPlan
	halt := ""
	for _, it := range rt.Items {
		if !it.IsCall() || it.Status != repo.ItemInProgress {
			continue
		}
		if halt != "" {
			plan.actions = append(plan.actions, callAction{kind: actionSkip, item: it, reason: "skipped: the turn stopped before this call was executed"})
			continue
		}
		h, pending, ready, err := rt.handleCall(ctx, it)
		if err != nil {
			return callPlan{}, err
		}
		switch {
		case pending != nil:
			plan.actions = append(plan.actions, callAction{kind: actionWait, item: *pending})
			return plan, nil
		case ready != nil:
			kind := actionRun
			if rt.readOnly(ready.it.Tool) {
				kind = actionRunReadOnly
			}
			plan.actions = append(plan.actions, callAction{kind: kind, item: ready.it, job: ready})
		case h != "":
			plan.actions = append(plan.actions, callAction{kind: actionHalt, reason: h})
			halt = h
		}
	}
	return plan, nil
}

func (rt *Runtime) readOnly(tool string) bool {
	d, ok := rt.tool(tool)
	return ok && d.Annotations.ReadOnlyHint && !d.Serial
}

// handleCall decides what one in-progress call item needs next. At most one of
// pending and ready is returned: pending means the call waits for the user
// (approval), ready carries the work to execute.
func (rt *Runtime) handleCall(ctx context.Context, it repo.ItemRow) (string, *repo.ItemRow, *job, error) {
	if it.Await != nil {
		return "", nil, nil, nil
	}
	if it.Approval != nil {
		return rt.decided(it)
	}
	if _, offered := rt.tool(it.Tool); !offered {
		h, err := rt.refuse(it, fmt.Sprintf("tool %q is not available in this turn; use an exact tool name from the tool list", it.Tool))
		return h, nil, nil, err
	}
	args, err := callArguments(it)
	if err != nil {
		h, err := rt.refuse(it, err.Error())
		return h, nil, nil, err
	}
	descriptor, _ := rt.tool(it.Tool)
	if err := tools.ValidateArguments(args, descriptor.InputSchema); err != nil {
		h, err := rt.refuse(it, fmt.Sprintf("invalid arguments for %s: %v", it.Tool, err))
		return h, nil, nil, err
	}
	// The limit is a hard stop: check it before anything with side effects,
	// so the loop cannot be pushed past it by expensive prepares or approvals.
	if rt.toolCalls() >= maxToolCalls {
		if err := rt.fail(it, "skipped: the tool call limit for this turn was reached"); err != nil {
			return "", nil, nil, err
		}
		return fmt.Sprintf("reached the tool call limit (%d)", maxToolCalls), nil, nil, nil
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
	annotations := tools.Annotations{}
	if it.Annotations != nil {
		annotations = *it.Annotations
	}
	call := permission.Call{Tool: it.Tool, Annotations: annotations, Content: content, HasContent: hasContent, Prepared: it.PreparedExecution}
	decision := rt.Permissions.Decide(call, slices.Concat(rt.History, rt.Items))
	if decision.Verdict == permission.Deny {
		h, err := rt.refuse(it, decision.Reason)
		return h, nil, nil, err
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
			h, err := rt.refuse(it, fmt.Sprintf("the approved call has invalid arguments: %v", err))
			return h, nil, nil, err
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

// toolCalls counts every call item that has been handled: executed, failed,
// declined, skipped, or still waiting. Refused calls count too, otherwise the
// model could push the turn past the limit with invalid calls.
func (rt *Runtime) toolCalls() int {
	n := 0
	for _, it := range rt.Items {
		if !it.IsCall() {
			continue
		}
		if it.Status == repo.ItemInProgress && it.Await == nil && it.Approval == nil {
			continue
		}
		n++
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
