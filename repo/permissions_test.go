package repo

import (
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestWorkspacePermissionsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	w := WorkspaceRow{
		ID: "ws1", Name: "proj", RootPath: "/proj",
		ApprovalPolicy: "never",
		Permissions:    PermissionRules{Allow: []string{"shell"}, Deny: []string{"mcp"}},
		CreatedAt:      now, UpdatedAt: now,
	}
	added, err := db.AddWorkspace(w, func(existing WorkspaceRow) bool { return existing.ID == w.ID }, nil)
	if err != nil {
		t.Fatalf("add workspace: %v", err)
	}
	read, err := db.Workspace("ws1")
	if err != nil || read.ApprovalPolicy != "never" || len(read.Permissions.Allow) != 1 || len(read.Permissions.Deny) != 1 {
		t.Fatalf("workspace permissions not persisted: %+v err=%v", read, err)
	}
	changed, err := db.UpdateWorkspace("ws1", func(w WorkspaceRow) (WorkspaceRow, error) {
		w.ApprovalPolicy = "on-request"
		w.Permissions = PermissionRules{Ask: []string{"shell"}}
		return w, nil
	})
	if err != nil || changed.ApprovalPolicy != "on-request" || len(changed.Permissions.Ask) != 1 || len(changed.Permissions.Allow) != 0 {
		t.Fatalf("workspace permissions not updated: %+v err=%v", changed, err)
	}
	_ = added
}
