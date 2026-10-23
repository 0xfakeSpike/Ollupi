package permission

import (
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/repo"
)

type Checker struct {
	policy Policy
	rules  ruleSet
}

func New(policy Policy, rules Rules, registry *tools.Registry) (*Checker, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	set, err := rules.parse()
	if err != nil {
		return nil, err
	}
	catalog := registry.Descriptors()
	for _, r := range set.all() {
		if !slices.ContainsFunc(catalog, func(d tools.ToolDescriptor) bool { return d.Name == r.tool }) {
			return nil, fmt.Errorf("permission rule %q: tool %q is not in the tool registry", r.text, r.tool)
		}
		if r.pattern != nil && !registry.HasPermissionContent(r.tool) {
			return nil, fmt.Errorf("permission rule %q: %s takes no specifier; write %q to match every call", r.text, r.tool, r.tool)
		}
	}
	return &Checker{policy: policy, rules: set}, nil
}

func (c *Checker) Offers(tool string) bool {
	return !c.rules.removes(tool)
}

type Call struct {
	Tool        string
	Annotations tools.Annotations
	Content     string
	HasContent  bool
	Prepared    *tools.Prepared
}

type Verdict int

const (
	Run Verdict = iota
	Ask
	Deny
)

type Decision struct {
	Verdict Verdict
	Reason  string
	Rule    string
}

func (c *Checker) Decide(call Call, history []repo.ItemRow) Decision {
	if p := call.Prepared; p != nil && p.Escalated && !c.policy.allowsEscalation() {
		return Decision{Verdict: Deny, Reason: fmt.Sprintf("refused: under the approval policy %q nobody can approve running outside the sandbox; do the work inside the sandbox, or tell the user what needs to run outside it", c.policy)}
	}
	switch verdict, rule := c.rules.decide(call.Tool, call.Content, call.HasContent); verdict {
	case ruleDeny:
		return Decision{Verdict: Deny, Rule: rule, Reason: fmt.Sprintf("denied by the permission rule %s; do not try it again, find another way or tell the user", rule)}
	case ruleAsk:
		return Decision{Verdict: Ask, Rule: rule, Reason: fmt.Sprintf("Your permission rule %s asks before this call.", rule)}
	case ruleAllow:
		return Decision{Verdict: Run, Rule: rule}
	}
	if allowedForConversation(call, history) {
		return Decision{Verdict: Run, Reason: "allowed for this conversation earlier"}
	}
	if c.policy.requiresApproval(call.Annotations, call.Prepared) {
		d := Decision{Verdict: Ask}

		if p := call.Prepared; p != nil && p.Escalated {
			d.Reason = "Runs outside the sandbox: " + p.Justification
		}
		return d
	}
	return Decision{Verdict: Run}
}

func (d Decision) Approval(id string, call Call, at time.Time) *repo.Approval {
	a := &repo.Approval{ID: id, RequestedAt: at, Reason: d.Reason, Rule: d.Rule}
	if call.HasContent {
		a.Content = call.Content
	}
	return a
}

func allowedForConversation(call Call, history []repo.ItemRow) bool {
	for _, it := range history {
		if !it.IsCall() || it.Tool != call.Tool || it.Approval == nil || it.Approval.Decision != repo.DecisionAcceptForSession {
			continue
		}
		if !reflect.DeepEqual(it.PreparedExecution, call.Prepared) {
			continue
		}
		if !call.HasContent || it.Approval.Content == call.Content {
			return true
		}
	}
	return false
}
