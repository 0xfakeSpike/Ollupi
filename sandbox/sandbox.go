package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Config describes a sandbox. Paths may be relative or start with ~/.
//
// There is one sandbox: macOS Seatbelt, which Codex and Claude Code use on
// macOS too. It enforces the policy on the user's own machine and
// toolchain, needs nothing installed, and can express what a container
// cannot (credentials denied inside a writable folder, git metadata
// read-only wherever a repository is). ModeFullAccess runs without it.
type Config struct {
	// Mode is a Mode; empty is ModeWorkspaceWrite.
	Mode string
	// Network lets sandboxed commands connect out. With Domains set, only
	// to those hosts, through the sandbox's proxy; otherwise anywhere.
	Network bool
	// Domains, when it allows any host and Network is on, limits the
	// network to it. With Network off it is unused.
	Domains DomainRules
	// Roots are the command directories, writable in workspace-write mode.
	Roots []string
	// DenyRead adds to DefaultDenyRead.
	DenyRead []string
	// ReadOnly paths stay readable but not writable inside the roots.
	ReadOnly []string
	// ProtectedNames add to DefaultProtectedNames: names of files the
	// caller's program trusts, kept read-only wherever they appear below a
	// root (an agent's AGENTS.md, for one).
	ProtectedNames []string
	// EnvMarker, when set, names the environment variable set to "seatbelt"
	// in sandboxed commands, as Codex sets CODEX_SANDBOX, so their tests can
	// skip what the sandbox does not allow; EnvMarker+"_NETWORK_DISABLED" is
	// "1" when the network is off. Unsandboxed commands get neither.
	EnvMarker string
	// ExtraWritable adds to DefaultWritableCaches.
	ExtraWritable []string
	// UnixSockets are sockets, or directories of sockets, that commands may
	// connect to when Network is on. All others stay closed.
	UnixSockets []string
	// EnvPassthrough names host environment variables commands may see.
	EnvPassthrough []string
	// MaxOutputBytes caps stdout and stderr each; 0 is DefaultMaxOutputBytes.
	MaxOutputBytes int
}

// Command is one invocation. Program is an absolute path. Cwd is inside one
// of the command's roots.
type Command struct {
	Program string
	Args    []string
	Cwd     string
	Env     map[string]string
	Timeout time.Duration
	// Roots replace Config.Roots as the writable command directories for
	// this call (a turn bound to one project). They must be canonical and
	// pass ValidateRoot. Empty uses Config.Roots.
	Roots []string
	// Unsandboxed runs the command without the sandbox, after the user
	// approved leaving it. The environment allowlist still applies.
	Unsandboxed bool
}

// Result is what a command did. A non-zero ExitCode is a result, not an
// error: the model needs the output of a failing test. A command killed by
// a signal has ExitCode 128+signal and Signal set, as a shell reports it.
// Denials lists what the sandbox stopped the command from doing, such as
// "touch: file-write-create /Users/u/.zshrc" or "network: blocked
// example.com:443 (not in allowed_domains)", so the model can tell a
// sandbox failure from a bug.
type Result struct {
	Stdout, Stderr string
	ExitCode       int
	Signal         string
	Truncated      bool
	Denials        []string
}

// Kinds name how commands run, for execution records.
const (
	KindSeatbelt    = "macos-seatbelt"
	KindUnsandboxed = "unsandboxed"
)

// seatbeltSupported reports why this host cannot run the sandbox, if it
// cannot. A variable so tests of platform-independent logic can run anywhere.
var seatbeltSupported = defaultSeatbeltSupported

func defaultSeatbeltSupported() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("the sandbox needs macOS (this host is %s)", runtime.GOOS)
	}
	return nil
}

// ResolveMode returns the mode a sandbox runs with, "" picking
// workspace-write, or an error when this host cannot enforce it. Config
// validation calls it too, so Settings cannot save what New would reject.
// It fails closed: off macOS, running unsandboxed must be chosen.
func ResolveMode(mode string) (Mode, error) {
	m := ModeWorkspaceWrite
	if mode != "" {
		var err error
		if m, err = ParseMode(mode); err != nil {
			return "", err
		}
	}
	if m != ModeFullAccess {
		if err := seatbeltSupported(); err != nil {
			return "", fmt.Errorf("sandbox: mode %q: %w; set the mode to %q to run commands without a sandbox", m, err, ModeFullAccess)
		}
	}
	return m, nil
}

// Sandbox runs commands under one configuration.
type Sandbox struct {
	home           string
	policy         Policy
	domains        DomainRules
	envPassthrough []string
	maxOutputBytes int
	denials        *denialLog
	envMarker      string
}

func New(c Config) (*Sandbox, error) {
	if len(c.Roots) == 0 {
		return nil, fmt.Errorf("sandbox: at least one command directory is required")
	}
	mode, err := ResolveMode(c.Mode)
	if err != nil {
		return nil, err
	}
	if mode != ModeFullAccess && runtime.GOOS == "darwin" {
		if _, err := os.Stat(SeatbeltExecutable); err != nil {
			return nil, fmt.Errorf("sandbox: %w", err)
		}
	}
	if c.MaxOutputBytes < 0 {
		return nil, fmt.Errorf("sandbox: max output bytes must be >= 0")
	}
	if err := ValidateEnvNames(c.EnvPassthrough); err != nil {
		return nil, err
	}
	if c.EnvMarker != "" {
		if err := ValidateEnvNames([]string{c.EnvMarker}); err != nil {
			return nil, err
		}
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		if home, err = CanonicalPath(home, ""); err != nil {
			return nil, err
		}
	}
	canonical := func(what string, paths []string) ([]string, error) {
		out := make([]string, 0, len(paths))
		for _, path := range paths {
			p, err := CanonicalPath(path, home)
			if err != nil {
				return nil, fmt.Errorf("sandbox: %s: %w", what, err)
			}
			out = append(out, p)
		}
		return out, nil
	}
	roots, err := canonical("command directory", c.Roots)
	if err != nil {
		return nil, err
	}
	if mode != ModeFullAccess {
		for _, root := range roots {
			if err := ValidateRoot(root, home); err != nil {
				return nil, err
			}
		}
	}
	denyRead, err := canonical("deny_read", append(DefaultDenyRead(home), c.DenyRead...))
	if err != nil {
		return nil, err
	}
	readOnly, err := canonical("read_only", c.ReadOnly)
	if err != nil {
		return nil, err
	}
	extraWritable, err := canonical("extra_writable", append(DefaultWritableCaches(home), c.ExtraWritable...))
	if err != nil {
		return nil, err
	}
	sockets, err := canonical("unix_sockets", c.UnixSockets)
	if err != nil {
		return nil, err
	}
	for _, path := range sockets {
		if _, err := seatbeltPathRegex(path); err != nil {
			return nil, err
		}
	}
	var domains DomainRules
	if domains.Allowed, err = ParseDomains(c.Domains.Allowed); err != nil {
		return nil, err
	}
	if domains.Denied, err = ParseDomains(c.Domains.Denied); err != nil {
		return nil, err
	}
	if len(domains.Allowed) == 0 && len(domains.Denied) > 0 {
		return nil, fmt.Errorf("sandbox: denied_domains needs allowed_domains; without it the network is either off or open")
	}
	s := &Sandbox{
		home: home,
		policy: Policy{
			Mode: mode, Network: c.Network, WritableRoots: roots, ExtraWritable: extraWritable,
			DenyRead: denyRead, ReadOnly: readOnly, UnixSockets: sockets,
			ProtectedNames: append(append([]string(nil), DefaultProtectedNames...), c.ProtectedNames...),
		},
		domains:        domains,
		envPassthrough: append([]string(nil), c.EnvPassthrough...),
		maxOutputBytes: c.MaxOutputBytes,
		envMarker:      c.EnvMarker,
	}
	if err := s.policy.Validate(); err != nil {
		return nil, err
	}
	if s.maxOutputBytes == 0 {
		s.maxOutputBytes = DefaultMaxOutputBytes
	}
	if mode != ModeFullAccess && runtime.GOOS == "darwin" {
		// Without the log monitor commands still run, but a failure no
		// longer says it was the sandbox.
		l := newDenialLog()
		if err := l.start(); err == nil {
			s.denials = l
		}
	}
	return s, nil
}

// Close stops the denial log monitor.
func (s *Sandbox) Close() error {
	if s.denials != nil {
		s.denials.close()
	}
	return nil
}

// proxied reports whether the network goes through the allowlist proxy.
func (s *Sandbox) proxied() bool { return s.policy.Network && len(s.domains.Allowed) > 0 }

// Mode is the sandbox mode commands run with.
func (s *Sandbox) Mode() Mode { return s.policy.Mode }

// Enforced reports whether commands run under the sandbox, that is, in any
// mode but full-access.
func (s *Sandbox) Enforced() bool { return s.policy.Mode != ModeFullAccess }

// Kind is KindSeatbelt or KindUnsandboxed.
func (s *Sandbox) Kind() string {
	if s.Enforced() {
		return KindSeatbelt
	}
	return KindUnsandboxed
}

// Roots are the configured command directories, canonical.
func (s *Sandbox) Roots() []string { return append([]string(nil), s.policy.WritableRoots...) }

// Fingerprint is every setting that changes what a command may do, so that
// a command approved under one sandbox cannot run under a looser one.
func (s *Sandbox) Fingerprint() any {
	return struct {
		Policy         Policy
		Domains        DomainRules
		EnvPassthrough []string
		MaxOutputBytes int
	}{s.policy, s.domains, s.envPassthrough, s.maxOutputBytes}
}

// Summary tells the model the limits up front, so it does not burn calls
// discovering them.
func (s *Sandbox) Summary() string {
	if !s.Enforced() {
		return "No sandbox: commands run with the user's full access to files and network"
	}
	var fs string
	switch s.policy.Mode {
	case ModeReadOnly:
		fs = "Sandbox: files are read-only"
	default:
		fs = "Sandbox: writes are allowed only inside the command directories, temp dirs, and package/build caches; " +
			"git config and hooks, and the settings files that other programs later run (" + protectedSummary(s.policy.ProtectedNames) + "), are read-only; " +
			"credential files cannot be read"
	}
	switch {
	case s.proxied():
		allowed := strings.Join(s.domains.Allowed, ", ")
		if len(allowed) > 300 {
			allowed = allowed[:300] + ", ..."
		}
		return fs + "; network reaches only these hosts, through a proxy set in HTTP_PROXY, HTTPS_PROXY and ALL_PROXY: " + allowed +
			"; local Unix sockets (Docker, ssh-agent) are closed"
	case s.policy.Network:
		return fs + "; network is on, local Unix sockets (Docker, ssh-agent) are closed"
	}
	return fs + "; network is off"
}

// protectedSummary names a few protected files, for the model: the
// defaults' best-known ones and every name the caller added.
func protectedSummary(names []string) string {
	shown := append([]string{".vscode", ".envrc", ".husky"}, names[len(DefaultProtectedNames):]...)
	return strings.Join(shown, ", ") + ", ..."
}

// Check reports whether dir can be a command directory of a turn.
func (s *Sandbox) Check(dir string) error {
	if !s.Enforced() {
		return nil
	}
	return ValidateRoot(dir, s.home)
}

// commandRoots returns the roots c runs with under a sandbox, checking ones
// it brings.
func (s *Sandbox) commandRoots(c Command) ([]string, error) {
	if len(c.Roots) == 0 {
		return s.policy.WritableRoots, nil
	}
	for _, root := range c.Roots {
		canonical, err := CanonicalPath(root, s.home)
		if err != nil {
			return nil, err
		}
		if canonical != root {
			return nil, fmt.Errorf("sandbox: command directory %q is not canonical (%q)", root, canonical)
		}
		if err := ValidateRoot(root, s.home); err != nil {
			return nil, err
		}
	}
	return c.Roots, nil
}

// Run runs c to completion. It returns an error when the command cannot
// start, the sandbox fails, or the command runs past its timeout.
func (s *Sandbox) Run(ctx context.Context, c Command) (Result, error) {
	if c.Program == "" || c.Timeout <= 0 {
		return Result{}, fmt.Errorf("sandbox: incomplete command")
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	if !s.Enforced() || c.Unsandboxed {
		cmd := exec.CommandContext(ctx, c.Program, c.Args...)
		cmd.Dir = c.Cwd
		cmd.Env = Environ(os.Environ(), s.envPassthrough, c.Env)
		return runProcess(ctx, cmd, c.Program, c.Timeout, s.maxOutputBytes)
	}
	roots, err := s.commandRoots(c)
	if err != nil {
		return Result{}, err
	}
	policy := s.policy
	policy.WritableRoots = roots
	env := map[string]string{}
	if s.envMarker != "" {
		env[s.envMarker] = "seatbelt"
		if !policy.Network {
			env[s.envMarker+"_NETWORK_DISABLED"] = "1"
		}
	}
	var px *proxy
	if s.proxied() {
		if px, err = startProxy(s.domains); err != nil {
			return Result{}, err
		}
		defer px.close()
		policy.ProxyPort = px.port()
		for k, v := range px.env() {
			env[k] = v
		}
		// The JVM options combine with the model's own.
		if own := c.Env["JAVA_TOOL_OPTIONS"]; own != "" {
			env["JAVA_TOOL_OPTIONS"] = own + " " + env["JAVA_TOOL_OPTIONS"]
		}
		// git over SSH is not routed: the sandbox keeps ~/.ssh and
		// ssh-agent out of reach, so an SSH remote needs escalation, or
		// an https:// remote.
	}
	var tagID string
	if s.denials != nil && s.denials.running() {
		tagID, policy.LogTag = s.denials.newTag()
	}
	args, err := SeatbeltArgs(policy, SeatbeltTempDirs(), append([]string{c.Program}, c.Args...))
	if err != nil {
		if tagID != "" {
			s.denials.collect(tagID, false)
		}
		return Result{}, err
	}
	// The model's environment wins over ours, except for the variables
	// that route traffic and identify the sandbox.
	merged := make(map[string]string, len(c.Env)+len(env))
	for k, v := range c.Env {
		merged[k] = v
	}
	for k, v := range env {
		merged[k] = v
	}
	cmd := exec.CommandContext(ctx, SeatbeltExecutable, args...)
	cmd.Dir = c.Cwd
	cmd.Env = Environ(os.Environ(), s.envPassthrough, merged)
	res, err := runProcess(ctx, cmd, c.Program, c.Timeout, s.maxOutputBytes)
	// Denials are reported for failed commands only: a command that
	// succeeded coped with them (a cache it could not write, telemetry it
	// could not send), and listing them would only send the model looking.
	failed := err != nil || res.ExitCode != 0
	var denials []string
	if tagID != "" {
		denials = s.denials.collect(tagID, failed)
	}
	if px != nil {
		denials = append(denials, px.blockedHosts()...)
	}
	if !failed {
		denials = nil
	}
	if err != nil {
		if len(denials) > 0 {
			err = fmt.Errorf("%w; the sandbox denied: %s", err, strings.Join(denials, "; "))
		}
		return Result{}, err
	}
	res.Denials = denials
	return res, nil
}
