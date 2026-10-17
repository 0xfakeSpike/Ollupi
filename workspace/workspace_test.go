package workspace_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xfakeSpike/ollupi/execution"
	"github.com/0xfakeSpike/ollupi/storage"
	"github.com/0xfakeSpike/ollupi/workspace"
)

func TestRealDirectoryDeduplicationAndArchiveRestore(t *testing.T) {
	db, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := workspace.New(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	w, err := m.Add(project, "Project")
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.Add(alias, "")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != w.ID || len(m.List()) != 1 {
		t.Fatalf("alias created duplicate: %+v", m.List())
	}
	renamed, err := m.Rename(w.ID, "Renamed")
	if err != nil || renamed.Name != "Renamed" {
		t.Fatalf("rename: %+v %v", renamed, err)
	}
	if _, err := m.Archive(w.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := m.RequireActive(w.ID); err == nil {
		t.Fatal("archived project accepted")
	}
	restored, err := m.Add(alias, "Ignored existing name")
	if err != nil || restored.ArchivedAt != nil || restored.Name != "Renamed" {
		t.Fatalf("restore: %+v %v", restored, err)
	}
	if err := m.RequireActive(w.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := workspace.New(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.List()) != 1 || loaded.List()[0].Name != "Renamed" {
		t.Fatal("changes not persisted")
	}
	// Callers cannot mutate the manager's timestamp pointers.
	archived, err := m.Archive(w.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	original := *archived.ArchivedAt
	*archived.ArchivedAt = original.AddDate(1, 0, 0)
	copy, _ := m.Get(w.ID)
	if !copy.ArchivedAt.Equal(original) {
		t.Fatal("caller changed archived timestamp")
	}
}

func TestInvalidAndMovedProjectNeverFallsBack(t *testing.T) {
	db, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := workspace.New(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", file, project + "-missing"} {
		if _, err := m.Add(path, ""); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	w, err := m.Add(project, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rename(w.ID, "   "); err == nil {
		t.Fatal("accepted empty project name")
	}
	if err := os.Rename(project, project+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := m.RequireActive(w.ID); err == nil {
		t.Fatal("missing directory accepted")
	}
	if err := os.Symlink(project+"-moved", project); err != nil {
		t.Fatal(err)
	}
	if err := m.RequireActive(w.ID); err == nil {
		t.Fatal("retargeted directory accepted")
	}
	moved, err := m.Add(project+"-moved", "Moved project")
	if err != nil {
		t.Fatal(err)
	}
	canonicalMoved, err := execution.CanonicalDirectory(project + "-moved")
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID == w.ID || moved.RootPath != canonicalMoved || len(m.List()) != 2 {
		t.Fatalf("new canonical location reused invalid workspace: original=%+v moved=%+v", w, moved)
	}
	if err := m.RequireActive(moved.ID); err != nil {
		t.Fatalf("new location is not usable: %v", err)
	}
	if err := m.RequireActive(w.ID); err == nil {
		t.Fatal("adding moved project repaired historical execution directory")
	}
	duplicate, err := m.Add(project, "")
	if err != nil || duplicate.ID != moved.ID {
		t.Fatalf("redirect alias did not reuse new stable workspace: %+v %v", duplicate, err)
	}
	if err := m.RequireActive("unknown"); err == nil {
		t.Fatal("unknown project accepted")
	}
}

func TestCaseAliasOnCaseInsensitiveVolume(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "ProjectCase")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "projectcase")
	if _, err := os.Stat(alias); os.IsNotExist(err) {
		t.Skip("case-sensitive filesystem")
	}
	db, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := workspace.New(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Add(path, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Add(alias, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("case alias created duplicate project: %s %s", first.ID, second.ID)
	}
}

// A directory the command sandbox refuses is refused as a project when it
// is added, not later when a turn starts.
func TestAddRefusesWhatTheRootCheckRefuses(t *testing.T) {
	db, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	refused, err := execution.CanonicalDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := workspace.New(db, func(root string) error {
		if root == refused {
			return errors.New("cannot be a command directory")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(refused, "Home"); err == nil || len(m.List()) != 0 {
		t.Fatalf("refused directory was added: %v %+v", err, m.List())
	}
	if _, err := m.Add(t.TempDir(), "Project"); err != nil {
		t.Fatal(err)
	}
}
