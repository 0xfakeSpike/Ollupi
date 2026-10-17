package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// InstructionsFile holds standing instructions for the agent: the app's
// global ones, a project's, and those of a project's subdirectories.
const InstructionsFile = "AGENTS.md"

// MaxInstructionRunes bounds one instructions file so it cannot crowd out
// the prompt.
const MaxInstructionRunes = 6000

// ReadInstructions returns the trimmed instructions in dir, or "" when the
// file does not exist. An unreadable, non-UTF-8, or oversized file is an
// error: partial rules can invert their meaning.
func ReadInstructions(dir string) (string, error) {
	path := filepath.Join(dir, InstructionsFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("instructions: read %s: %w", path, err)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("instructions: %s is not valid UTF-8", path)
	}
	text := strings.TrimSpace(string(data))
	if n := utf8.RuneCountInString(text); n > MaxInstructionRunes {
		return "", fmt.Errorf("instructions: %s has %d characters; at most %d are allowed", path, n, MaxInstructionRunes)
	}
	return text, nil
}

// dependencyDirs hold third-party packages, which ship AGENTS.md files of
// their own; those are not the project's instructions.
var dependencyDirs = map[string]bool{"node_modules": true, "vendor": true, "Pods": true, ".git": true}

// SubdirectoryInstruction is the AGENTS.md of one directory below a
// command root.
type SubdirectoryInstruction struct {
	// Path is the file's canonical path.
	Path    string
	Content string
	// Digest identifies the content, so an edited file is loaded again.
	Digest string
}

// SubdirectoryInstructions returns the instructions files that apply to a
// command run in cwd and that the turn's snapshot does not hold: those of
// the directories from the command root containing cwd (exclusive, its own
// file is the project's) down to cwd (inclusive), outermost first, as
// Claude Code loads nested CLAUDE.md files. Directories inside dependency
// folders are skipped. cwd must be canonical and inside one of c's roots.
func SubdirectoryInstructions(c *Context, cwd string) ([]SubdirectoryInstruction, error) {
	root := ""
	for _, r := range c.CwdRoots {
		if Contains(r, cwd) && len(r) > len(root) {
			root = r
		}
	}
	if root == "" {
		return nil, fmt.Errorf("instructions: %q is outside this turn's directories", cwd)
	}
	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == "." {
		return nil, err
	}
	snapshot := map[string]bool{}
	for _, in := range c.Instructions {
		snapshot[filepath.Clean(in.Path)] = true
	}
	var out []SubdirectoryInstruction
	dir := root
	for _, name := range strings.Split(rel, string(filepath.Separator)) {
		if dependencyDirs[name] {
			return out, nil
		}
		dir = filepath.Join(dir, name)
		path := filepath.Join(dir, InstructionsFile)
		if snapshot[path] {
			continue
		}
		text, err := ReadInstructions(dir)
		if err != nil {
			return nil, err
		}
		if text == "" {
			continue
		}
		sum := sha256.Sum256([]byte(text))
		out = append(out, SubdirectoryInstruction{Path: path, Content: text, Digest: "sha256:" + hex.EncodeToString(sum[:])})
	}
	return out, nil
}
