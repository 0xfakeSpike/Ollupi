package shell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/pkg/sandbox"
)

func (r *Runner) Prepare(ctx context.Context, args map[string]any) (*tools.Prepared, error) {
	d, err := r.dirs(tools.CallerOf(ctx))
	if err != nil {
		return nil, err
	}
	program, ok := args["program"].(string)
	if !ok || strings.TrimSpace(program) == "" {
		return nil, fmt.Errorf("shell: argument program must be a non-empty string")
	}
	program = strings.TrimSpace(program)
	argv, err := parseExecArgsSlice(args["args"])
	if err != nil {
		return nil, err
	}
	requested := ""
	if raw, ok := args["work_dir"]; ok {
		requested, ok = raw.(string)
		if !ok || strings.TrimSpace(requested) == "" {
			return nil, fmt.Errorf("shell: argument work_dir must be a non-empty string")
		}
	}
	cwd, err := d.resolve(requested)
	if err != nil {
		return nil, err
	}
	argv = normalizeShellArgv(program, cwd, argv)
	for _, blocked := range r.blacklist {
		if blacklisted(program, argv, blocked) {
			return nil, fmt.Errorf("shell: command is forbidden by the blacklist entry %q: %s", blocked, strings.Join(append([]string{program}, argv...), " "))
		}
	}
	timeout := r.timeoutSec
	if raw, ok := args["timeout_sec"]; ok {
		timeout, err = parseTimeoutSec(raw, r.maxTimeoutSec)
		if err != nil {
			return nil, err
		}
	}
	env, err := commandEnvironment(args["env"])
	if err != nil {
		return nil, err
	}
	escalated, justification, err := r.escalation(args)
	if err != nil {
		return nil, err
	}
	program, err = resolveExecutable(program, cwd)
	if err != nil {
		return nil, err
	}
	return &tools.Prepared{
		Cwd: cwd, SettingsDigest: r.settingsDigest(d), ArgumentsDigest: digest(args),
		Program: program, Args: append([]string(nil), argv...), Env: env, TimeoutSec: timeout,
		Sandboxed: r.box.Enforced() && !escalated, Escalated: escalated, Justification: justification,
	}, nil
}

func (r *Runner) escalation(args map[string]any) (bool, string, error) {
	mode := SandboxUseDefault
	if raw, ok := args["sandbox_permissions"]; ok {
		s, ok := raw.(string)
		if !ok || (s != SandboxUseDefault && s != SandboxRequireEscalated) {
			return false, "", fmt.Errorf("shell: argument sandbox_permissions must be %q or %q", SandboxUseDefault, SandboxRequireEscalated)
		}
		mode = s
	}
	justification := ""
	if raw, ok := args["justification"]; ok {
		s, ok := raw.(string)
		if !ok {
			return false, "", fmt.Errorf("shell: argument justification must be a string")
		}
		justification = strings.TrimSpace(s)
	}
	if mode != SandboxRequireEscalated {
		return false, "", nil
	}
	if !r.box.Enforced() {
		return false, "", fmt.Errorf("shell: commands already run without a sandbox here; omit sandbox_permissions")
	}
	if justification == "" {
		return false, "", fmt.Errorf("shell: sandbox_permissions %q needs a justification for the user", SandboxRequireEscalated)
	}
	return true, justification, nil
}

func commandEnvironment(raw any) (map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	values, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("shell: argument env must be an object")
	}

	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		value, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("shell: env value for %q must be string", k)
		}
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("shell: invalid environment variable %q", k)
		}
		out[k] = value
	}
	return out, nil
}

func resolveExecutable(program, cwd string) (string, error) {
	var path string
	var err error
	if strings.ContainsRune(program, os.PathSeparator) {
		path = program
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
	} else {
		path, err = exec.LookPath(program)
		if err != nil {
			return "", fmt.Errorf("shell: executable %q is not on PATH", program)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("shell: executable %q is unavailable: %w", program, err)
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if st.IsDir() || st.Mode()&0111 == 0 {
		return "", fmt.Errorf("shell: %q is not executable", program)
	}
	return resolved, nil
}

func (r *Runner) Run(ctx context.Context, args map[string]any, p *tools.Prepared) (tools.Result, error) {
	fail := func(err error) (tools.Result, error) { return tools.Result{Err: err}, err }
	d, err := r.dirs(tools.CallerOf(ctx))
	if err != nil {
		return fail(err)
	}
	if err := r.validatePrepared(d, p, args); err != nil {
		return fail(err)
	}
	if p.Program == "" || p.TimeoutSec <= 0 {
		return fail(fmt.Errorf("shell: incomplete prepared command"))
	}

	current, err := resolveExecutable(p.Program, p.Cwd)
	if err != nil {
		return fail(err)
	}
	if current != p.Program {
		return fail(fmt.Errorf("shell: approved executable now resolves to a different path"))
	}
	res, err := r.box.Run(ctx, sandbox.Command{
		Program: p.Program, Args: p.Args, Cwd: p.Cwd, Env: p.Env,
		Timeout: time.Duration(p.TimeoutSec) * time.Second,

		Roots:       d.roots,
		Unsandboxed: p.Escalated,
	})
	if err != nil {
		return fail(err)
	}
	out := map[string]any{"stdout": res.Stdout, "stderr": res.Stderr, "exit_code": res.ExitCode, "work_dir": p.Cwd}
	if res.Signal != "" {
		out["signal"] = res.Signal
	}
	if res.Truncated {
		out["truncated"] = true
	}
	if len(res.Denials) > 0 {
		out["sandbox_denials"] = res.Denials
	}
	if p.Escalated {
		out["sandbox"] = "escalated: ran on the host without the sandbox"
	}
	return tools.Result{Output: out}, nil
}

func (r *Runner) validatePrepared(d dirs, p *tools.Prepared, args map[string]any) error {
	if p == nil {
		return fmt.Errorf("shell: missing prepared command")
	}
	if p.SettingsDigest != r.settingsDigest(d) {
		return fmt.Errorf("shell: the command settings or the project directory changed after this call was prepared, so it was not run; call run_command again")
	}
	if p.ArgumentsDigest != digest(args) {
		return fmt.Errorf("shell: prepared command arguments changed")
	}
	cwd, err := d.resolve(p.Cwd)
	if err != nil {
		return err
	}
	if cwd != p.Cwd {
		return fmt.Errorf("shell: prepared command directory changed")
	}
	return nil
}
