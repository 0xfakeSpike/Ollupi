// Package sandbox runs commands under macOS Seatbelt: it describes what a
// command may touch (Policy), turns that into a profile, and supervises the
// process (process group, output cap, environment allowlist, a domain
// allowlist proxy, denial reports). It knows nothing about the program that
// uses it: what else to protect (Config.ProtectedNames, ReadOnly, DenyRead)
// and how to mark its commands (Config.EnvMarker) come from the caller.
package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mode is how much of the filesystem a command may change. The three levels
// follow Codex's sandbox modes.
type Mode string

const (
	// ModeReadOnly may read files but not write any.
	ModeReadOnly Mode = "read-only"
	// ModeWorkspaceWrite may write inside its writable roots and temp dirs.
	ModeWorkspaceWrite Mode = "workspace-write"
	// ModeFullAccess is not sandboxed. DenyRead does not apply.
	ModeFullAccess Mode = "full-access"
)

func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.TrimSpace(s)); m {
	case ModeReadOnly, ModeWorkspaceWrite, ModeFullAccess:
		return m, nil
	default:
		return "", fmt.Errorf("sandbox: mode must be %q, %q, or %q", ModeReadOnly, ModeWorkspaceWrite, ModeFullAccess)
	}
}

// Policy is what one command may do. Paths are absolute and canonical.
type Policy struct {
	Mode Mode
	// Network allows outbound and inbound connections, localhost included.
	// Unix sockets stay closed except those inside WritableRoots and
	// UnixSockets: a socket is a door to a process outside the sandbox
	// (Docker's daemon, ssh-agent), not to the network.
	Network bool
	// WritableRoots are writable in ModeWorkspaceWrite, along with temp dirs.
	WritableRoots []string
	// ExtraWritable are also writable in ModeWorkspaceWrite but are not
	// command directories: package and build caches (DefaultWritableCaches).
	ExtraWritable []string
	// DenyRead paths may be neither read nor written, even inside a writable
	// root (credentials, the app's own config and database).
	DenyRead []string
	// ReadOnly paths may be read but not changed, even inside a writable
	// root: files that later run or steer something outside the sandbox.
	// Git metadata is always protected this way; see seatbeltGitRules.
	ReadOnly []string
	// ProtectedNames are file and directory names kept read-only wherever
	// they appear below a writable root, outside dependency folders:
	// DefaultProtectedNames and the caller's.
	ProtectedNames []string
	// UnixSockets may be connected to when Network is on.
	UnixSockets []string
	// ProxyPort, when set with Network, limits the network to the sandbox's
	// proxy on this loopback port, which enforces the domain allowlist.
	ProxyPort int
	// LogTag, when set, is attached to the profile's denials so the
	// sandbox's log monitor can tell this command's denials apart.
	LogTag string
}

func (p Policy) Validate() error {
	if _, err := ParseMode(string(p.Mode)); err != nil {
		return err
	}
	if p.Mode == ModeWorkspaceWrite && len(p.WritableRoots) == 0 {
		return fmt.Errorf("sandbox: %s needs at least one writable root", ModeWorkspaceWrite)
	}
	if p.ProxyPort < 0 || p.ProxyPort > 65535 || p.ProxyPort != 0 && !p.Network {
		return fmt.Errorf("sandbox: invalid proxy port %d", p.ProxyPort)
	}
	for _, r := range p.LogTag {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			return fmt.Errorf("sandbox: invalid log tag %q", p.LogTag)
		}
	}
	for _, name := range p.ProtectedNames {
		if err := validProtectedName(name); err != nil {
			return err
		}
	}
	for _, list := range [][]string{p.WritableRoots, p.ExtraWritable, p.DenyRead, p.ReadOnly, p.UnixSockets} {
		for _, path := range list {
			if !filepath.IsAbs(path) || filepath.Clean(path) != path {
				return fmt.Errorf("sandbox: path %q must be absolute and clean", path)
			}
		}
	}
	return nil
}

// ValidateRoot rejects command directories that would make the sandbox
// pointless: the filesystem root, the home directory or one of its
// ancestors, and ~/Library. Writing there means writing shell startup files
// and LaunchAgents, which run outside the sandbox. root and home are
// canonical.
func ValidateRoot(root, home string) error {
	if root == "/" {
		return fmt.Errorf("sandbox: / cannot be a command directory")
	}
	if home == "" {
		return nil
	}
	for _, protected := range []string{home, filepath.Join(home, "Library")} {
		if within(root, protected) {
			return fmt.Errorf("sandbox: %q cannot be a command directory because it contains %s; choose a project folder inside it", root, protected)
		}
	}
	return nil
}

// DefaultProtectedNames are files and directories that stay read-only
// wherever they appear under a command directory, because a program outside
// the sandbox later runs what they say: editors run tasks and interpreters
// named in their settings, direnv and mise run .envrc and their config when
// you cd into the folder, and hook managers (husky, pre-commit, lefthook)
// run hooks on your next commit. A sandboxed command writing them would run
// code outside the sandbox later. The list follows sandbox-runtime's
// (Claude Code) and Codex's; a caller adds the files its own program trusts
// (Config.ProtectedNames), such as an agent's instruction files.
var DefaultProtectedNames = []string{
	// editors and IDEs
	".vscode", ".idea", ".zed", ".fleet", ".devcontainer",
	// shells and environment managers
	".envrc", ".mise.toml", "mise.toml", ".mise",
	".bashrc", ".bash_profile", ".zshrc", ".zprofile", ".zshenv", ".profile",
	// git hooks outside .git, and git configuration in the work tree
	".husky", ".githooks", ".pre-commit-config.yaml", "lefthook.yml", ".lefthook.yml", "lefthook.yaml",
	".gitconfig", ".gitmodules",
	// other tools that run configured commands
	".ripgreprc",
}

// validProtectedName accepts a plain file or directory name: the profile
// matches it with a regex, so only characters that need no quoting.
func validProtectedName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("sandbox: invalid protected name %q", name)
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
			return fmt.Errorf("sandbox: protected name %q may hold only letters, digits, dots, dashes, and underscores", name)
		}
	}
	return nil
}

// DefaultDenyRead lists credential stores and private data under home that
// commands never need for workspace work. The rule for the list: build and
// package tools do not read these on an ordinary run, so denying them does
// not break builds. ~/.npmrc, ~/.m2/settings.xml, ~/.gradle and gh's token
// are read on ordinary runs and so are absent; add them with exec.deny_read
// when commands do not need them. With the network off, a readable secret
// cannot leave the machine from inside the sandbox.
func DefaultDenyRead(home string) []string {
	if home == "" {
		return nil
	}
	names := []string{
		// keys and cloud credentials
		".ssh", ".gnupg", ".aws", ".azure", ".kube", ".netrc", ".config/gcloud", ".docker/config.json",
		".git-credentials", ".cargo/credentials", ".cargo/credentials.toml", ".pypirc", ".gem/credentials",
		".vault-token", ".terraform.d/credentials.tfrc.json", ".password-store", ".config/op", ".codex/auth.json",
		// shell history often holds pasted tokens
		".zsh_history", ".bash_history", ".python_history", ".node_repl_history",
		// macOS keychains, browsers, and messages
		"Library/Keychains", "Library/Cookies", "Library/Messages", "Library/Mail", "Library/Safari",
		"Library/Application Support/Google/Chrome", "Library/Application Support/Firefox",
		"Library/Application Support/BraveSoftware", "Library/Application Support/Microsoft Edge",
		"Library/Application Support/Arc",
	}
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = filepath.Join(home, name)
	}
	return out
}

// DefaultWritableCaches lists the per-user caches that everyday build tools
// write to (go, npm, pip, uv, cargo, Homebrew, CocoaPods). Without them a
// workspace-write sandbox breaks `go build` and `npm install`. They hold
// downloaded packages and build output, not credentials.
//
// The trade-off: a sandboxed command can change what a later build outside
// the sandbox compiles from these caches. Directories whose contents run
// directly outside the sandbox stay out: ~/Library/pnpm itself (pnpm's
// global bin, on PATH; only its store is here), ~/.cache as a whole
// (pre-commit environments, corepack's package managers), and Playwright's
// browsers.
func DefaultWritableCaches(home string) []string {
	if home == "" {
		return nil
	}
	names := []string{
		"Library/Caches/go-build", "go/pkg/mod",
		".npm", "Library/Caches/Yarn", "Library/pnpm/store", "Library/Caches/node-gyp",
		"Library/Caches/pip", "Library/Caches/pypoetry", ".cache/pip", ".cache/uv",
		".cargo/registry", ".cargo/git", ".rustup/downloads", ".rustup/tmp",
		"Library/Caches/Homebrew", "Library/Caches/CocoaPods",
	}
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = filepath.Join(home, name)
	}
	return out
}

// CanonicalPath expands a leading ~/, makes path absolute, and resolves
// symlinks of its longest existing prefix, so /tmp/x matches /private/tmp/x
// whether or not x exists yet.
func CanonicalPath(path, home string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("sandbox: empty path")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home == "" {
			return "", fmt.Errorf("sandbox: %q needs a home directory", path)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	existing, rest := abs, ""
	for {
		resolved, err := filepath.EvalSymlinks(existing)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("sandbox: resolve %q: %w", abs, err)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
