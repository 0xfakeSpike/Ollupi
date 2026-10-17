package thread

import (
	"fmt"
	"strings"
)

type PlanStatus string

// Plan step statuses, as in Codex's update_plan tool.
const (
	PlanPending    PlanStatus = "pending"
	PlanInProgress PlanStatus = "in_progress"
	PlanCompleted  PlanStatus = "completed"
)

const maxPlanSteps = 12

// PlanStep is one step of the model-maintained plan.
type PlanStep struct {
	Step   string     `json:"step"`
	Status PlanStatus `json:"status"`
}

// Plan is the model's todo list (Codex update_plan). Every update replaces
// the whole list; earlier versions remain in the record.
type Plan struct {
	Explanation string     `json:"explanation,omitempty"`
	Steps       []PlanStep `json:"plan"`
}

// ValidatePlan checks a proposed plan before it is recorded.
func ValidatePlan(p Plan) error {
	if len(p.Steps) == 0 || len(p.Steps) > maxPlanSteps {
		return fmt.Errorf("plan requires 1 to %d steps", maxPlanSteps)
	}
	inProgress := 0
	for i, step := range p.Steps {
		if strings.TrimSpace(step.Step) == "" {
			return fmt.Errorf("plan step %d is empty", i+1)
		}
		switch step.Status {
		case PlanPending, PlanCompleted:
		case PlanInProgress:
			inProgress++
		default:
			return fmt.Errorf("plan step %d has invalid status %q", i+1, step.Status)
		}
	}
	if inProgress > 1 {
		return fmt.Errorf("at most one plan step can be in_progress")
	}
	return nil
}
