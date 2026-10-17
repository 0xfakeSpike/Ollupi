package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// Permissions are permission rules in Claude Code's syntax. A rule is a tool
// name, which matches every call of the tool, or Tool(specifier) for tools
// that take one (see tools.PermissionContentProvider). A specifier is a glob
// over the call's content where * matches anything; a trailing " *" or the
// older ":*" also matches the bare prefix, at a word boundary: run_command(git
// diff *) matches "git diff" and "git diff --stat" but not "git difftool".
// Tool(*) is the same as Tool.
//
// Rules are checked deny first, then ask, then allow; the first match
// decides. A deny rule refuses the call; a bare tool name in deny removes the
// tool from the agent. An ask rule always asks for approval, whatever the
// approval policy. An allow rule runs the call without asking. Calls that
// match no rule follow the approval policy.
type Permissions struct {
	Allow []string
	Ask   []string
	Deny  []string
}

// permission is what the rules decide for one call.
type permission int

const (
	// permissionDefault: no rule matched; the approval policy decides.
	permissionDefault permission = iota
	permissionDeny
	permissionAsk
	permissionAllow
)

// rule is one parsed permission rule.
type rule struct {
	text string
	tool string
	// pattern matches the call's content; nil matches every call.
	pattern *regexp.Regexp
}

func parseRule(text string) (rule, error) {
	s := strings.TrimSpace(text)
	r := rule{text: s, tool: s}
	if open := strings.IndexByte(s, '('); open >= 0 {
		if !strings.HasSuffix(s, ")") {
			return rule{}, fmt.Errorf("permission rule %q: a specifier must end with )", text)
		}
		r.tool = s[:open]
		spec := s[open+1 : len(s)-1]
		switch {
		case strings.TrimSpace(spec) == "":
			return rule{}, fmt.Errorf("permission rule %q: empty specifier; write the tool name alone to match every call", text)
		case spec != "*":
			r.pattern = specifierPattern(spec)
		}
	}
	if r.tool == "" || strings.ContainsAny(r.tool, " \t()") {
		return rule{}, fmt.Errorf("permission rule %q: expected Tool or Tool(specifier)", text)
	}
	return r, nil
}

// specifierPattern compiles a specifier glob into an anchored expression.
func specifierPattern(spec string) *regexp.Regexp {
	optionalTail := false
	switch {
	case strings.HasSuffix(spec, ":*"):
		spec, optionalTail = strings.TrimSuffix(spec, ":*"), true
	case strings.HasSuffix(spec, " *"):
		spec, optionalTail = strings.TrimSuffix(spec, " *"), true
	}
	parts := strings.Split(spec, "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	expr := "^" + strings.Join(parts, ".*")
	if optionalTail {
		expr += "(?: .*)?"
	}
	return regexp.MustCompile(expr + "$")
}

// matches reports whether the rule covers a call of tool whose content
// (meaningful only when hasContent) is content.
func (r rule) matches(tool, content string, hasContent bool) bool {
	if r.tool != tool {
		return false
	}
	if r.pattern == nil {
		return true
	}
	return hasContent && r.pattern.MatchString(content)
}

// ruleSet is the parsed Permissions.
type ruleSet struct {
	allow, ask, deny []rule
}

func (p Permissions) parse() (ruleSet, error) {
	var set ruleSet
	for _, list := range []struct {
		name  string
		texts []string
		into  *[]rule
	}{{"deny", p.Deny, &set.deny}, {"ask", p.Ask, &set.ask}, {"allow", p.Allow, &set.allow}} {
		for _, text := range list.texts {
			r, err := parseRule(text)
			if err != nil {
				return ruleSet{}, fmt.Errorf("permissions.%s: %w", list.name, err)
			}
			*list.into = append(*list.into, r)
		}
	}
	return set, nil
}

// all lists every rule.
func (s ruleSet) all() []rule {
	return append(append(append([]rule(nil), s.deny...), s.ask...), s.allow...)
}

// removes reports whether a bare deny rule removes tool from the agent.
func (s ruleSet) removes(tool string) bool {
	for _, r := range s.deny {
		if r.tool == tool && r.pattern == nil {
			return true
		}
	}
	return false
}

// decide returns what the rules say about a call, and the rule that said it.
func (s ruleSet) decide(tool, content string, hasContent bool) (permission, string) {
	for _, group := range []struct {
		rules []rule
		kind  permission
	}{{s.deny, permissionDeny}, {s.ask, permissionAsk}, {s.allow, permissionAllow}} {
		for _, r := range group.rules {
			if r.matches(tool, content, hasContent) {
				return group.kind, r.text
			}
		}
	}
	return permissionDefault, ""
}
