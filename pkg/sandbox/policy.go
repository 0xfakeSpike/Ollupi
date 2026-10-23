package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Mode string

const (
	ModeReadOnly       Mode = "read-only"
	ModeWorkspaceWrite Mode = "workspace-write"
	ModeFullAccess     Mode = "full-access"
)

func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.TrimSpace(s)); m {
	case ModeReadOnly, ModeWorkspaceWrite, ModeFullAccess:
		return m, nil
	default:
		return "", fmt.Errorf("sandbox: mode must be %q, %q, or %q", ModeReadOnly, ModeWorkspaceWrite, ModeFullAccess)
	}
}

type Policy struct {
	Mode           Mode
	Network        bool
	WritableRoots  []string
	ExtraWritable  []string
	DenyRead       []string
	ReadOnly       []string
	ProtectedNames []string
	UnixSockets    []string
	ProxyPort      int
	LogTag         string
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

func ValidateRoot(root, home string) error {
	if root == "/" {
		return fmt.Errorf("sandbox: / cannot be a command directory")
	}
	if home == "" {
		return nil
	}
	for _, protected := range []string{home, filepath.Join(home, "Library")} {
		if Within(root, protected) {
			return fmt.Errorf("sandbox: %q cannot be a command directory because it contains %s; choose a project folder inside it", root, protected)
		}
	}
	return nil
}

var DefaultProtectedNames = []string{
	".vscode", ".idea", ".zed", ".fleet", ".devcontainer",

	".envrc", ".mise.toml", "mise.toml", ".mise",
	".bashrc", ".bash_profile", ".zshrc", ".zprofile", ".zshenv", ".profile",

	".husky", ".githooks", ".pre-commit-config.yaml", "lefthook.yml", ".lefthook.yml", "lefthook.yaml",
	".gitconfig", ".gitmodules",

	".ripgreprc",
}

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

func DefaultDenyRead(home string) []string {
	if home == "" {
		return nil
	}
	names := []string{
		".ssh", ".gnupg", ".aws", ".azure", ".kube", ".netrc", ".config/gcloud", ".docker/config.json",
		".git-credentials", ".cargo/credentials", ".cargo/credentials.toml", ".pypirc", ".gem/credentials",
		".vault-token", ".terraform.d/credentials.tfrc.json", ".password-store", ".config/op", ".codex/auth.json",

		".zsh_history", ".bash_history", ".python_history", ".node_repl_history",

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

func CanonicalDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("directory is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("directory %q is unavailable: %w", abs, err)
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("directory %q is unavailable: %w", resolved, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%q is not a directory", resolved)
	}
	return filepath.Clean(resolved), nil
}

func Within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
