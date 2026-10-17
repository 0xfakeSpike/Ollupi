package agent

import (
	"testing"

	"github.com/0xfakeSpike/ollupi/execution"
	"github.com/0xfakeSpike/ollupi/tools"
)

// Commands follow Codex: inside the sandbox they run without asking under
// on-request; leaving it asks; untrusted asks for every command.
func TestApprovalOfSandboxedAndEscalatedCommands(t *testing.T) {
	command := tools.Annotations{DestructiveHint: tools.Hint(true), OpenWorldHint: tools.Hint(true)}
	sandboxed := &execution.Prepared{Sandboxed: true}
	escalated := &execution.Prepared{Escalated: true, Justification: "push"}
	host := &execution.Prepared{}
	for _, tc := range []struct {
		policy   ApprovalPolicy
		prepared *execution.Prepared
		want     bool
	}{
		{ApprovalOnRequest, sandboxed, false},
		{ApprovalOnRequest, escalated, true},
		{ApprovalOnRequest, host, true},
		{ApprovalOnRequest, nil, true},
		{ApprovalUntrusted, sandboxed, true},
		{ApprovalUntrusted, escalated, true},
		{ApprovalNever, escalated, false},
		{ApprovalNever, host, false},
	} {
		if got := tc.policy.RequiresApproval(command, tc.prepared); got != tc.want {
			t.Errorf("%s with %+v: requires approval = %v", tc.policy, tc.prepared, got)
		}
	}
	if ApprovalNever.AllowsEscalation() || !ApprovalOnRequest.AllowsEscalation() || !ApprovalUntrusted.AllowsEscalation() {
		t.Error("only never forbids escalation")
	}
}
