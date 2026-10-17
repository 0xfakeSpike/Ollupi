package agent

import (
	"fmt"

	"github.com/0xfakeSpike/ollupi/execution"

	"github.com/0xfakeSpike/ollupi/tools"
)

// ApprovalPolicy decides which tool calls need the user's approval, named
// after Codex's approval policies. Most tools are judged by their MCP
// annotations. A local command that runs inside an enforcing sandbox
// (execution.Prepared.Sandboxed) is judged by the sandbox instead, as in
// Codex: on-request runs it without asking, and the model escalates a
// command that needs to leave the sandbox, which then asks.
type ApprovalPolicy string

const (
	// ApprovalUntrusted runs only read-only tools that stay inside the local
	// workspace (readOnlyHint and openWorldHint=false); everything else
	// asks, sandboxed commands included.
	ApprovalUntrusted ApprovalPolicy = "untrusted"
	// ApprovalOnRequest runs read-only tools and sandboxed commands, and
	// asks for every other call, escalated commands included.
	ApprovalOnRequest ApprovalPolicy = "on-request"
	// ApprovalNever never asks; every call runs. Commands cannot escalate
	// out of the sandbox, since nobody would approve it.
	ApprovalNever ApprovalPolicy = "never"
)

// ParseApprovalPolicy accepts exactly one of the policy names.
func ParseApprovalPolicy(name string) (ApprovalPolicy, error) {
	switch p := ApprovalPolicy(name); p {
	case ApprovalUntrusted, ApprovalOnRequest, ApprovalNever:
		return p, nil
	default:
		return "", fmt.Errorf("approval policy %q must be %q, %q, or %q", name, ApprovalUntrusted, ApprovalOnRequest, ApprovalNever)
	}
}

// RequiresApproval reports whether a call to a tool with these annotations
// must be approved before it runs. prepared is the call's frozen local
// execution, nil for tools without one.
func (p ApprovalPolicy) RequiresApproval(a tools.Annotations, prepared *execution.Prepared) bool {
	switch p {
	case ApprovalNever:
		return false
	case ApprovalOnRequest:
		if prepared != nil && prepared.Sandboxed && !prepared.Escalated {
			return false
		}
		return !a.ReadOnlyHint
	case ApprovalUntrusted:
		return !a.ReadOnlyHint || tools.OpenWorld(a)
	default:
		panic(fmt.Sprintf("agent: unparsed approval policy %q", p))
	}
}

// AllowsEscalation reports whether a command may ask to leave the sandbox.
func (p ApprovalPolicy) AllowsEscalation() bool { return p != ApprovalNever }
