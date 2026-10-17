// Package execution defines the immutable, persisted execution environment of
// a turn. A workspace selects command directories; it is not a filesystem sandbox.
package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Version is the format of Context and Prepared records. Version 2 names the
// sandbox (Sandbox, SandboxDigest) where version 1 named a backend.
const Version = 2

type Instruction struct {
	Scope   string `json:"scope"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

type Context struct {
	Version    int      `json:"version"`
	RootPath   string   `json:"rootPath,omitempty"`
	DefaultCwd string   `json:"defaultCwd"`
	CwdRoots   []string `json:"cwdRoots"`
	// Sandbox is how commands run: sandbox.KindSeatbelt or
	// sandbox.KindUnsandboxed. SandboxDigest covers every sandbox and command
	// setting, so a turn whose settings changed cannot run its approved
	// commands under different ones.
	Sandbox       string        `json:"sandbox"`
	SandboxDigest string        `json:"sandboxDigest"`
	Instructions  []Instruction `json:"instructions"`
}

// Prepared is persisted before a local command is sent for approval or run.
// Program, Args, Env and TimeoutSec describe the actual invocation after provider
// normalization; ArgumentsDigest binds that invocation to the original tool input.
// It intentionally does not capture the host's complete environment or file contents.
type Prepared struct {
	Version                int               `json:"version"`
	Cwd                    string            `json:"cwd"`
	ExecutionContextDigest string            `json:"executionContextDigest"`
	ArgumentsDigest        string            `json:"argumentsDigest"`
	Program                string            `json:"program,omitempty"`
	Args                   []string          `json:"args,omitempty"`
	Env                    map[string]string `json:"env,omitempty"`
	TimeoutSec             int               `json:"timeoutSec,omitempty"`
	// Sandboxed is set when the command will run inside a sandbox that
	// limits its files and network. The on-request approval policy runs
	// such commands without asking, as Codex does.
	Sandboxed bool `json:"sandboxed,omitempty"`
	// Escalated is set when the model asked to run the command outside the
	// sandbox; Justification is its reason, shown to the user who approves.
	Escalated     bool   `json:"escalated,omitempty"`
	Justification string `json:"justification,omitempty"`
}

// Digest is deterministic for these JSON-compatible records. It is a consistency
// check, not an authorization token or a signature.
func Digest(value any) string {
	body, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("execution: digest non-JSON value: %v", err))
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func Clone(c *Context) *Context {
	if c == nil {
		return nil
	}
	out := *c
	out.CwdRoots = append([]string(nil), c.CwdRoots...)
	out.Instructions = append([]Instruction(nil), c.Instructions...)
	return &out
}

// CanonicalDirectory resolves symlinks and requires an existing directory.
func CanonicalDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("execution: directory is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("execution: resolve directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("execution: directory %q is unavailable: %w", abs, err)
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("execution: directory %q is unavailable: %w", resolved, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("execution: %q is not a directory", resolved)
	}
	return filepath.Clean(resolved), nil
}

func Contains(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// ResolveCwd resolves a requested directory relative to the turn's default and
// checks the real path against its allowed cwd roots, including symlink targets.
func ResolveCwd(c *Context, requested string) (string, error) {
	if err := Validate(c); err != nil {
		return "", err
	}
	path := requested
	if path == "" {
		path = c.DefaultCwd
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(c.DefaultCwd, path)
	}
	resolved, err := CanonicalDirectory(path)
	if err != nil {
		return "", err
	}
	for _, root := range c.CwdRoots {
		if Contains(root, resolved) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("execution: directory %q is outside this turn's allowed directories", requested)
}

// Validate checks a saved environment without silently canonicalizing it into a
// different directory. A disappeared or retargeted project must fail on recovery.
func Validate(c *Context) error {
	if c == nil {
		return fmt.Errorf("execution: missing execution context")
	}
	if c.Version != Version {
		return fmt.Errorf("execution: unsupported context version %d", c.Version)
	}
	if c.Sandbox == "" || c.SandboxDigest == "" || len(c.CwdRoots) == 0 {
		return fmt.Errorf("execution: incomplete execution context")
	}
	paths := append([]string{c.DefaultCwd}, c.CwdRoots...)
	if c.RootPath != "" {
		paths = append(paths, c.RootPath)
	}
	for _, path := range paths {
		canonical, err := CanonicalDirectory(path)
		if err != nil {
			return err
		}
		if canonical != path {
			return fmt.Errorf("execution: saved directory %q now resolves to %q; reopen the project before running it", path, canonical)
		}
	}
	for _, root := range c.CwdRoots {
		if Contains(root, c.DefaultCwd) {
			return nil
		}
	}
	return fmt.Errorf("execution: default directory is outside the saved allowed directories")
}

func ValidatePrepared(c *Context, p *Prepared, arguments map[string]any) error {
	if p == nil || p.Version != Version {
		return fmt.Errorf("execution: missing or unsupported prepared execution")
	}
	if p.ExecutionContextDigest != Digest(c) {
		return fmt.Errorf("execution: prepared command belongs to a different execution context")
	}
	if p.ArgumentsDigest != Digest(arguments) {
		return fmt.Errorf("execution: prepared command arguments changed")
	}
	cwd, err := ResolveCwd(c, p.Cwd)
	if err != nil {
		return err
	}
	if cwd != p.Cwd {
		return fmt.Errorf("execution: prepared command directory changed")
	}
	return nil
}

type workspaceIDKey struct{}

// WithWorkspaceID carries thread ownership to tools such as cron. Ownership is
// not duplicated inside the execution snapshot or command records.
func WithWorkspaceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, workspaceIDKey{}, id)
}
func WorkspaceID(ctx context.Context) string {
	id, _ := ctx.Value(workspaceIDKey{}).(string)
	return id
}
