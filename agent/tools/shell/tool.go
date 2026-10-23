package shell

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/pkg/sandbox"
)

const ToolName = "run_command"

type Options struct {
	Config    Config
	Agent     AgentFiles
	DenyRead  []string
	EnvMarker string
}

const (
	SandboxUseDefault       = "use_default"
	SandboxRequireEscalated = "require_escalated"
)

type Runner struct {
	box           *sandbox.Sandbox
	timeoutSec    int
	maxTimeoutSec int
	blacklist     []string
}

func NewRunner(o Options) (*Runner, error) {
	c := o.Config
	c.WorkspaceDirs = append([]string(nil), c.WorkspaceDirs...)
	if err := c.Normalize(o.Agent.Home); err != nil {
		return nil, fmt.Errorf("shell: %w", err)
	}
	cfg := sandboxConfig(o, c)
	cfg.Roots = make([]string, 0, len(c.WorkspaceDirs))
	for _, r := range c.WorkspaceDirs {
		abs, err := sandbox.CanonicalDirectory(r)
		if err != nil {
			return nil, fmt.Errorf("shell: workspace dir: %w", err)
		}
		cfg.Roots = append(cfg.Roots, abs)
	}
	box, err := sandbox.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("shell: %w", err)
	}
	return &Runner{box: box, timeoutSec: c.CommandTimeoutSec, maxTimeoutSec: c.MaxCommandTimeoutSec, blacklist: append([]string(nil), c.CommandBlacklist...)}, nil
}

func (r *Runner) Close() error { return r.box.Close() }

func (r *Runner) Name() (string, string) {
	return "shell", "Run commands in the working folders, under the macOS sandbox unless it is turned off."
}

func (r *Runner) tool() *tools.Spec {
	desc := "Run a command: set arguments.program to argv0 (a binary on PATH or an absolute path). work_dir defaults to the project root, or outside a project to " + r.box.Roots()[0] +
		", and must stay inside the project (outside a project: inside " + strings.Join(r.box.Roots(), ", ") + "). " + r.box.Summary() +
		". A non-zero exit_code is returned with the output, not as an error"
	if r.box.Enforced() {
		desc += ". Commands run in the sandbox without asking the user. A result's sandbox_denials lists what the sandbox stopped. " +
			"When a command fails because of the sandbox (a write outside the command directories or to a protected file, " +
			"a host outside the allowed domains, the network, a local socket), " +
			"retry it with sandbox_permissions \"require_escalated\" and a one-sentence justification: " +
			"the user is asked, and an approved command runs on the host without the sandbox. " +
			"Do not escalate to get around a denied path on purpose, and do not escalate commands that work in the sandbox"
	}

	return &tools.Spec{
		Name:        ToolName,
		Description: desc,
		InputSchema: r.inputSchema(),
		Annotations: &tools.Annotations{DestructiveHint: tools.Hint(true), OpenWorldHint: tools.Hint(true)},

		PermissionContent: commandLine,
		Local:             &tools.Local{Prepare: r.Prepare, Run: r.Run},
	}
}

func commandLine(arguments map[string]any) (string, error) {
	program, ok := arguments["program"].(string)
	if !ok || strings.TrimSpace(program) == "" {
		return "", fmt.Errorf("shell: argument \"program\" must be non-empty string")
	}
	args, err := parseExecArgsSlice(arguments["args"])
	if err != nil {
		return "", err
	}
	env, err := commandEnvironment(arguments["env"])
	if err != nil {
		return "", err
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	words := make([]string, 0, len(keys)+1+len(args))
	for _, k := range keys {
		words = append(words, k+"="+env[k])
	}
	words = append(words, strings.TrimSpace(program))
	return strings.Join(append(words, args...), " "), nil
}

func (r *Runner) Invoke(context.Context, string, map[string]any) (tools.Result, error) {
	err := fmt.Errorf("shell: run_command runs only inside a turn")
	return tools.Result{Err: err}, err
}

func ToolInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"program": map[string]any{
				"type":        "string",
				"description": "argv0: executable name or path (e.g. opencli, npm, git, sh, bash)",
			},
			"args": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "Argv after program. For sh/bash/zsh/dash: use [\"-c\", \"whole shell command\"] for one-liners; " +
					"a single arg is treated as a script path (sh myscript.sh). For other programs use normal argv.",
			},
			"work_dir": map[string]any{
				"type":        "string",
				"description": "Optional command directory, relative to the current project/default command directory; must stay within this turn's allowed directories",
			},
			"timeout_sec": map[string]any{
				"type":        "integer",
				"description": "Optional timeout in seconds",
			},
			"env": map[string]any{
				"type":                 "object",
				"description":          "Optional environment variables",
				"additionalProperties": map[string]any{"type": "string"},
			},
		},
		"required":             []string{"program"},
		"additionalProperties": false,
	}
}

func (r *Runner) inputSchema() map[string]any {
	schema := ToolInputSchema()
	if !r.box.Enforced() {
		return schema
	}
	props := schema["properties"].(map[string]any)
	props["sandbox_permissions"] = map[string]any{
		"type":        "string",
		"enum":        []string{SandboxUseDefault, SandboxRequireEscalated},
		"description": "use_default (the default) runs in the sandbox without asking; require_escalated asks the user to run this command outside the sandbox",
	}
	props["justification"] = map[string]any{
		"type":        "string",
		"description": "Required with require_escalated: one sentence telling the user why the command must run outside the sandbox",
	}
	return schema
}

func (r *Runner) ToolList(context.Context) ([]*tools.Spec, error) {
	return []*tools.Spec{r.tool()}, nil
}

func parseExecArgsSlice(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("shell: argument \"args\" must be array of strings")
	}
	out := make([]string, len(arr))
	for i, el := range arr {
		s, ok := el.(string)
		if !ok {
			return nil, fmt.Errorf("shell: argument \"args\"[%d] must be string", i)
		}
		out[i] = s
	}
	return out, nil
}

func parseTimeoutSec(v any, limit int) (int, error) {
	var sec int
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || t <= 0 {
			return 0, fmt.Errorf("shell: argument \"timeout_sec\" must be > 0")
		}
		if t > float64(limit) {
			return 0, fmt.Errorf("shell: argument \"timeout_sec\" must be at most %d", limit)
		}
		sec = max(1, int(math.Ceil(t)))
	case int:
		if t <= 0 {
			return 0, fmt.Errorf("shell: argument \"timeout_sec\" must be > 0")
		}
		sec = t
	default:
		return 0, fmt.Errorf("shell: argument \"timeout_sec\" must be integer seconds")
	}
	if sec > limit {
		return 0, fmt.Errorf("shell: argument \"timeout_sec\" must be at most %d", limit)
	}
	return sec, nil
}

func blacklisted(program string, argv []string, blocked string) bool {
	want := strings.Fields(blocked)
	if len(want) == 0 {
		return false
	}
	words := []string{filepath.Base(program)}
	for i, a := range argv {
		words = append(words, a)
		if a == "-c" && i+1 < len(argv) && isShell(program) {
			words = append(words, strings.Fields(argv[i+1])...)
		}
	}
	for i := 0; i+len(want) <= len(words); i++ {
		match := true
		for j, w := range want {
			got := words[i+j]
			if got != w && (j > 0 || strings.Contains(w, "/") || filepath.Base(got) != w) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func isShell(program string) bool {
	switch strings.ToLower(filepath.Base(strings.TrimSpace(program))) {
	case "sh", "bash", "zsh", "dash", "ash", "ksh":
		return true
	}
	return false
}

func normalizeShellArgv(tool, wd string, extra []string) []string {
	tool = strings.TrimSpace(tool)
	if tool == "" || len(extra) == 0 {
		return extra
	}
	if !isShell(tool) {
		return extra
	}
	if extra[0] == "-c" || extra[0] == "-s" {
		return extra
	}
	if len(extra) == 1 {
		return extra
	}
	first := extra[0]
	if shellFirstArgIsScriptFile(first, wd) {
		return extra
	}
	return []string{"-c", strings.Join(extra, " ")}
}

func shellFirstArgIsScriptFile(first, wd string) bool {
	if strings.Contains(first, "/") || strings.HasPrefix(first, "./") {
		return true
	}
	p := filepath.Join(wd, first)
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return true
	}
	if filepath.IsAbs(first) {
		if st, err := os.Stat(first); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}
