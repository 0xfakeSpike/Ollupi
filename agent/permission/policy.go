package permission

import (
	"fmt"

	"github.com/0xfakeSpike/ollupi/agent/tools"
)

type Policy string

const (
	Untrusted Policy = "untrusted"
	OnRequest Policy = "on-request"
	Never     Policy = "never"
)

func (p Policy) Validate() error {
	switch p {
	case Untrusted, OnRequest, Never:
		return nil
	default:
		return fmt.Errorf("approval policy %q must be %q, %q, or %q", p, Untrusted, OnRequest, Never)
	}
}

func (p Policy) requiresApproval(a tools.Annotations, prepared *tools.Prepared) bool {
	switch p {
	case Never:
		return false
	case OnRequest:
		if prepared != nil && prepared.Sandboxed && !prepared.Escalated {
			return false
		}
		return !a.ReadOnlyHint
	case Untrusted:
		return !a.ReadOnlyHint || tools.OpenWorld(a)
	default:
		panic(fmt.Sprintf("permission: unparsed approval policy %q", p))
	}
}

func (p Policy) allowsEscalation() bool { return p != Never }
