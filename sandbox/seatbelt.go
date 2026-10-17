package sandbox

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SeatbeltExecutable is macOS's sandbox launcher. Apple marks it deprecated,
// but it is what Codex and Claude Code use and it still works.
const SeatbeltExecutable = "/usr/bin/sandbox-exec"

var (
	//go:embed seatbelt_base.sbpl
	seatbeltBase string
	//go:embed seatbelt_network.sbpl
	seatbeltNetwork string
	//go:embed seatbelt_preferences.sbpl
	seatbeltPreferences string
)

// System libcurl needs this service for TLS (from Codex's seatbelt.rs).
const seatbeltTLSTrust = `(allow mach-lookup (global-name "com.apple.TrustEvaluationAgent"))`

// With the network on, every Unix socket that (allow network-outbound)
// opened is closed again, and mDNSResponder, which name resolution goes
// through, is reopened. Without this, network-on commands could reach any
// local daemon: Docker's socket (`docker run -v $HOME:/h` escapes the
// sandbox) or ssh-agent (signs with keys that ~/.ssh's deny_read was meant
// to keep out). Path filters such as subpath do not match socket connects
// reliably; regexes do.
const seatbeltMDNS = `(allow network-outbound (remote unix-socket (regex #"^/private/var/run/mDNSResponder$")))
`

// writeUnixSocketAllows opens the configured Unix sockets.
func writeUnixSocketAllows(b *strings.Builder, sockets []string) error {
	for _, path := range sockets {
		re, err := seatbeltPathRegex(path)
		if err != nil {
			return err
		}
		b.WriteString(`(allow network-outbound (remote unix-socket (regex #"` + re + `")))` + "\n")
	}
	return nil
}

// seatbeltGitRules keep the configuration and hooks of every repository
// under a writable root read-only. Git runs hooks and config-named programs
// (core.fsmonitor, core.hooksPath, aliases, filter drivers) the next time
// the user runs git outside the sandbox, so writing them is an escape.
// Commits, branches, and the index stay writable; git init, clone, and
// `git remote add` write config and need escalation. commondir and a .git
// file can point git at another directory, so .git cannot be removed or
// rewritten either. Each rule is an operation and a path pattern below a
// root; see rootRule. Repositories in temp dirs and caches are not covered,
// so package managers can still clone git dependencies there.
var seatbeltGitRules = []struct{ op, pattern string }{
	{"file-write*", `.git/config$`},
	{"file-write*", `.git/config.worktree$`},
	{"file-write*", `.git/commondir$`},
	{"file-write*", `.git/hooks(/|$)`},
	{"file-write*", `.git/modules/.+/config$`},
	{"file-write*", `.git/modules/.+/commondir$`},
	{"file-write*", `.git/modules/.+/hooks(/|$)`},
	{"file-write*", `.git/worktrees/[^/]+/config.worktree$`},
	{"file-write*", `.git/worktrees/[^/]+/commondir$`},
	{"file-write-unlink", `.git$`},
	{"file-write-data", `.git$`},
}

// rootRule is a deny rule for pattern anywhere below root, for regular
// files only when fileOnly. pattern is a regex whose dots are literal and
// whose letters match either case: APFS is case-insensitive by default, so
// .GIT/hooks is .git/hooks. A root the profile cannot quote gets a prefix
// that matches every directory: stricter, never looser.
func rootRule(op, root, pattern string, fileOnly bool, message string) string {
	prefix := "/"
	if quoted, err := seatbeltRegexQuote(root); err == nil {
		prefix = "^" + caseless(quoted) + "/(.*/)?"
	}
	filter := "(regex #\"" + prefix + caselessPattern(pattern) + "\")"
	if fileOnly {
		filter = "(require-all (vnode-type REGULAR-FILE) " + filter + ")"
	}
	return "(deny " + op + " " + filter + message + ")"
}

// caseless makes the letters of an already quoted regex match either case.
func caseless(quoted string) string {
	var b strings.Builder
	escaped := false
	for _, r := range quoted {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\':
			b.WriteRune(r)
			escaped = true
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			lower, upper := strings.ToLower(string(r)), strings.ToUpper(string(r))
			b.WriteString("[" + lower + upper + "]")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// caselessPattern turns a rule pattern into a regex: dots are literal,
// ".+", "[^/]+", "(/|$)" and "$" keep their meaning, letters are caseless.
func caselessPattern(pattern string) string {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case strings.HasPrefix(pattern[i:], ".+"):
			b.WriteString(".+")
			i++
		case strings.HasPrefix(pattern[i:], "[^/]+"):
			b.WriteString("[^/]+")
			i += 4
		case c == '.':
			b.WriteString("\\.")
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			b.WriteString("[" + strings.ToLower(string(c)) + strings.ToUpper(string(c)) + "]")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// protectedNamePattern matches a protected name and anything below it.
// The names are plain (letters, digits, dots, dashes, underscores).
func protectedNamePattern(name string) string { return name + "(/|$)" }

// dependencyDirs hold third-party packages that package managers unpack,
// and packages ship .vscode folders and AGENTS.md files of their own.
// Protected names do not apply below them, or `npm install` would fail;
// git metadata stays protected there too.
var dependencyDirs = []string{"node_modules", "vendor", "Pods"}

// SeatbeltArgs returns sandbox-exec arguments that run argv under p. Paths go
// in as -D parameters, never spliced into the profile text, except allowed
// Unix sockets, which need a regex and are quoted for it.
func SeatbeltArgs(p Policy, tempDirs []string, argv []string) ([]string, error) {
	profile, params, err := SeatbeltProfile(p, tempDirs)
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("sandbox: empty command")
	}
	args := []string{"-p", profile}
	for _, kv := range params {
		args = append(args, "-D", kv[0]+"="+kv[1])
	}
	args = append(args, "--")
	return append(args, argv...), nil
}

// SeatbeltProfile builds a deny-by-default profile: Codex's base policy, full
// read access, then the policy's writes and network, and finally the denies,
// which must come last because a later matching rule wins.
func SeatbeltProfile(p Policy, tempDirs []string) (string, [][2]string, error) {
	if err := p.Validate(); err != nil {
		return "", nil, err
	}
	if p.Mode == ModeFullAccess {
		return "", nil, fmt.Errorf("sandbox: %s is not run under a profile", ModeFullAccess)
	}
	var params [][2]string
	param := func(prefix string, i int, path string) string {
		key := fmt.Sprintf("%s_%d", prefix, i)
		params = append(params, [2]string{key, path})
		return fmt.Sprintf(`(param "%s")`, key)
	}

	var b strings.Builder
	b.WriteString("(version 1)\n")
	if p.LogTag != "" {
		// Tag the catch-all deny too, so its denials reach the log monitor.
		b.WriteString(strings.Replace(seatbeltBase, "(deny default)", `(deny default (with message "`+p.LogTag+`"))`, 1))
	} else {
		b.WriteString(seatbeltBase)
	}
	b.WriteString("\n; reads\n(allow file-read*)\n")
	b.WriteString(seatbeltPreferences)
	// confstr(_CS_DARWIN_USER_TEMP_DIR) asks dirhelper; git and xcrun fail without it.
	b.WriteString("\n(allow mach-lookup (global-name \"com.apple.bsd.dirhelper\"))\n")

	var writable []string
	if p.Mode == ModeWorkspaceWrite {
		writable = append(append(append(writable, p.WritableRoots...), p.ExtraWritable...), tempDirs...)
		b.WriteString("\n; writes\n(allow file-write*")
		for i, root := range writable {
			b.WriteString("\n  (subpath " + param("WRITABLE_ROOT", i, root) + ")")
		}
		b.WriteString(")\n")
	}

	msg := ""
	if p.LogTag != "" {
		msg = ` (with message "` + p.LogTag + `")`
	}
	// Creating a Unix socket is its own operation, with no path to check
	// (sandbox-runtime). It grants nothing by itself: connecting is checked
	// by path, below, and denied unless allowed. It lets socketpair work,
	// which Python's multiprocessing and many test runners use.
	b.WriteString("\n(allow system-socket (socket-domain AF_UNIX))\n")
	switch {
	case p.Network && p.ProxyPort != 0:
		// Only the sandbox's proxy, which enforces the domain allowlist.
		// Name resolution is the proxy's job too: an open resolver would
		// let a command leak data in the names it looks up.
		b.WriteString(fmt.Sprintf("\n; network: the sandbox's proxy only\n(allow network-outbound (remote ip \"localhost:%d\"))\n", p.ProxyPort))
		b.WriteString(seatbeltNetwork)
		b.WriteString("\n" + seatbeltTLSTrust + "\n")
		if err := writeUnixSocketAllows(&b, p.UnixSockets); err != nil {
			return "", nil, err
		}
	case p.Network:
		b.WriteString("\n; network\n(allow network-outbound)\n(allow network-inbound)\n")
		b.WriteString(seatbeltNetwork)
		b.WriteString("\n" + seatbeltTLSTrust + "\n")
		b.WriteString("\n; unix sockets (after the network allow)\n")
		b.WriteString("(deny network-outbound (remote unix-socket)" + msg + ")\n")
		b.WriteString(seatbeltMDNS)
		if err := writeUnixSocketAllows(&b, p.UnixSockets); err != nil {
			return "", nil, err
		}
	}

	b.WriteString("\n; denies (keep last)\n")
	b.WriteString("(deny mach-lookup (xpc-service-name-prefix \"\"))\n")
	if p.Mode == ModeWorkspaceWrite {
		for _, root := range p.WritableRoots {
			for _, name := range p.ProtectedNames {
				b.WriteString(rootRule("file-write*", root, protectedNamePattern(name), false, msg) + "\n")
			}
			for _, dir := range dependencyDirs {
				b.WriteString(strings.Replace(rootRule("file-write*", root, dir+"/", false, ""), "(deny ", "(allow ", 1) + "\n")
			}
			for _, rule := range seatbeltGitRules {
				// A .git file (worktree, submodule) points git elsewhere.
				fileOnly := rule.op == "file-write-data"
				b.WriteString(rootRule(rule.op, root, rule.pattern, fileOnly, msg) + "\n")
			}
		}
	}
	// After the dependency-folder allows, so those cannot reopen them.
	for i, path := range p.DenyRead {
		b.WriteString("(deny file-read* file-write* (subpath " + param("DENY_READ", i, path) + ")" + msg + ")\n")
	}
	for i, path := range p.ReadOnly {
		b.WriteString("(deny file-write* (subpath " + param("READ_ONLY", i, path) + ")" + msg + ")\n")
	}
	// Renaming a writable ancestor of a denied or read-only path would move
	// it to a path the denies no longer name.
	protected := append(append([]string(nil), p.DenyRead...), p.ReadOnly...)
	for i, dir := range protectedAncestors(protected, writable) {
		b.WriteString("(deny file-write-unlink (require-all (vnode-type DIRECTORY) (literal " + param("PROTECTED_ANCESTOR", i, dir) + ")))\n")
	}
	// These fcntls change files through read-only descriptors (Codex):
	// F_MAKECOMPRESSED = 80, F_TRANSFEREXTENTS = 110.
	b.WriteString("(deny system-fcntl (fcntl-command 80 110))\n")
	return b.String(), params, nil
}

// seatbeltPathRegex matches path and everything under it. Seatbelt has no
// parameterized regex, so the path is escaped instead, and a path that
// cannot be quoted safely is refused.
func seatbeltPathRegex(path string) (string, error) {
	quoted, err := seatbeltRegexQuote(path)
	if err != nil {
		return "", err
	}
	return "^" + quoted + "(/|$)", nil
}

// seatbeltRegexQuote escapes path for a #"..." regex literal.
func seatbeltRegexQuote(path string) (string, error) {
	for _, r := range path {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("sandbox: path %q contains a character the profile cannot quote", path)
		}
	}
	var b strings.Builder
	for _, r := range path {
		if strings.ContainsRune(`.[]()*+?{}|^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String(), nil
}

// protectedAncestors returns each directory between a protected path and
// the writable root containing it, the root included.
func protectedAncestors(denied, writable []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, path := range denied {
		for _, root := range writable {
			if !within(root, path) || path == root {
				continue
			}
			for dir := filepath.Dir(path); within(root, dir); dir = filepath.Dir(dir) {
				if !seen[dir] {
					seen[dir] = true
					out = append(out, dir)
				}
				if dir == root {
					break
				}
			}
		}
	}
	return out
}

// SeatbeltTempDirs are the temp directories a workspace-write command may
// use: /private/tmp and the per-user $TMPDIR under /private/var/folders.
func SeatbeltTempDirs() []string {
	dirs := []string{"/private/tmp"}
	if tmp, err := CanonicalPath(os.TempDir(), ""); err == nil && tmp != "/private/tmp" {
		dirs = append(dirs, tmp)
	}
	return dirs
}
