package execution

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubdirectoryInstructionsWalkFromRootToCwd(t *testing.T) {
	root, err := CanonicalDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(rel, text string) {
		t.Helper()
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if text != "" {
			if err := os.WriteFile(filepath.Join(dir, InstructionsFile), []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(".", "project")
	write("services", "services rules")
	write("services/api", "")
	write("services/api/handlers", "handler rules")
	write("web/node_modules/pkg", "a package's own rules")
	c := &Context{
		Version: Version, DefaultCwd: root, CwdRoots: []string{root}, Sandbox: "unsandboxed", SandboxDigest: "d",
		Instructions: []Instruction{{Scope: "project", Path: filepath.Join(root, InstructionsFile), Content: "project"}},
	}

	got, err := SubdirectoryInstructions(c, filepath.Join(root, "services/api/handlers"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Content != "services rules" || got[1].Content != "handler rules" {
		t.Fatalf("want services then handlers, got %+v", got)
	}
	if got[0].Path != filepath.Join(root, "services", InstructionsFile) || !strings.HasPrefix(got[0].Digest, "sha256:") {
		t.Fatalf("path or digest: %+v", got[0])
	}
	for _, cwd := range []string{root, filepath.Join(root, "web/node_modules/pkg")} {
		if got, err := SubdirectoryInstructions(c, cwd); err != nil || len(got) != 0 {
			t.Fatalf("%s: want nothing, got %+v %v", cwd, got, err)
		}
	}
	if _, err := SubdirectoryInstructions(c, t.TempDir()); err == nil {
		t.Fatal("a directory outside the roots must be refused")
	}
	write("broken", strings.Repeat("x", MaxInstructionRunes+1))
	if _, err := SubdirectoryInstructions(c, filepath.Join(root, "broken")); err == nil {
		t.Fatal("an oversized file must be an error, not skipped")
	}
}
