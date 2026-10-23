package shell

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/pkg/sandbox"
)

type dirs struct {
	roots []string
	cwd   string
}

func (r *Runner) dirs(caller tools.Caller) (dirs, error) {
	if caller.Root == "" {
		roots := r.box.Roots()
		return dirs{roots: roots, cwd: roots[0]}, nil
	}
	root, err := sandbox.CanonicalDirectory(caller.Root)
	if err != nil {
		return dirs{}, err
	}
	if root != caller.Root {
		return dirs{}, fmt.Errorf("shell: the project directory %q now resolves to %q; add its new location as a workspace", caller.Root, root)
	}
	if err := r.box.Check(root); err != nil {
		return dirs{}, err
	}
	return dirs{roots: []string{root}, cwd: root}, nil
}

func (d dirs) resolve(requested string) (string, error) {
	path := requested
	if path == "" {
		path = d.cwd
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(d.cwd, path)
	}
	resolved, err := sandbox.CanonicalDirectory(path)
	if err != nil {
		return "", err
	}
	for _, root := range d.roots {
		if sandbox.Within(root, resolved) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("shell: directory %q is outside the command directories", requested)
}

func (r *Runner) settingsDigest(d dirs) string {
	return digest(struct {
		Sandbox    any
		TimeoutSec int
		Blacklist  []string
		Roots      []string
	}{r.box.Fingerprint(), r.timeoutSec, r.blacklist, d.roots})
}

func digest(value any) string {
	body, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("shell: digest non-JSON value: %v", err))
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
