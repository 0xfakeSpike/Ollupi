package cron_test

import (
	"context"
	"testing"
	"time"

	"github.com/0xfakeSpike/ollupi/cron"
	"github.com/0xfakeSpike/ollupi/execution"
	"github.com/0xfakeSpike/ollupi/storage"
	"github.com/0xfakeSpike/ollupi/workspace"
)

func TestScheduledToolInheritsTrustedConversationWorkspace(t *testing.T) {
	db, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := workspace.New(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	project, err := projects.Add(t.TempDir(), "Project")
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := cron.New(db, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	scheduler.SetWorkspaceValidator(projects.RequireActive)
	provider := cron.NewProvider(scheduler)
	args := map[string]any{"id": "project-job", "spec": "@daily", "prompt": "inspect project"}
	ctx := execution.WithWorkspaceID(context.Background(), project.ID)
	if _, err := provider.Invoke(ctx, cron.ToolCreateJob, args); err != nil {
		t.Fatal(err)
	}
	jobs, err := db.JobsSelectAll()
	if err != nil || len(jobs) != 1 || jobs[0].WorkspaceID != project.ID {
		t.Fatalf("tool lost trusted project: %+v %v", jobs, err)
	}
	// Unscoped jobs stay general; the model cannot pick a project in tool input.
	args["id"] = "general-job"
	args["workspaceId"] = "fake-model-input"
	if _, err := provider.Invoke(context.Background(), cron.ToolCreateJob, args); err != nil {
		t.Fatal(err)
	}
	list := scheduler.List()
	for _, job := range list {
		if job.ID == "general-job" && job.WorkspaceID != "" {
			t.Fatal("tool trusted model workspace input")
		}
	}
	restored, err := cron.New(db, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	var fired cron.Job
	if err := restored.RunWith("project-job", func(job cron.Job) error { fired = job; return nil }); err != nil {
		t.Fatal(err)
	}
	if fired.WorkspaceID != project.ID {
		t.Fatal("restart lost scheduled project")
	}
	if _, err := projects.Archive(project.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.CreateInWorkspace("blocked", "@daily", "work", project.ID); err == nil {
		t.Fatal("new archived job accepted")
	}
	if _, err := scheduler.CreateInWorkspace("unknown", "@daily", "work", "missing"); err == nil {
		t.Fatal("unknown project accepted")
	}
}

func TestAgentJobListAndDeleteCannotCrossWorkspaces(t *testing.T) {
	db, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	projects, err := workspace.New(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := projects.Add(t.TempDir(), "A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := projects.Add(t.TempDir(), "B")
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := cron.New(db, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	scheduler.SetWorkspaceValidator(projects.RequireActive)
	owners := []string{a.ID, b.ID, ""}
	ids := []string{"a-job", "b-job", "general-job"}
	for i, owner := range owners {
		if _, err := scheduler.CreateInWorkspace(ids[i], "@daily", "work", owner); err != nil {
			t.Fatal(err)
		}
	}
	provider := cron.NewProvider(scheduler)
	for i, owner := range owners {
		ctx := execution.WithWorkspaceID(context.Background(), owner)
		result, err := provider.Invoke(ctx, cron.ToolListJobs, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		jobs := result.Output.(map[string]any)["jobs"].([]cron.JobStatus)
		if len(jobs) != 1 || jobs[0].ID != ids[i] {
			t.Fatalf("workspace %q saw another project's jobs: %+v", owner, jobs)
		}
		for j, id := range ids {
			if i != j {
				if _, err := provider.Invoke(ctx, cron.ToolDeleteJob, map[string]any{"id": id}); err == nil {
					t.Fatalf("workspace %q deleted %s", owner, id)
				}
			}
		}
	}
	if len(scheduler.List()) != 3 {
		t.Fatal("cross-project delete mutated jobs")
	}
	// Management API retains global visibility and deletion permission.
	if err := scheduler.Delete(ids[1]); err != nil {
		t.Fatal(err)
	}
	if len(scheduler.List()) != 2 {
		t.Fatal("management deletion failed")
	}
	if _, err := provider.Invoke(execution.WithWorkspaceID(context.Background(), a.ID), cron.ToolDeleteJob, map[string]any{"id": ids[0]}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Invoke(context.Background(), cron.ToolDeleteJob, map[string]any{"id": ids[2]}); err != nil {
		t.Fatal(err)
	}
	if len(scheduler.List()) != 0 {
		t.Fatal("owners could not delete own jobs")
	}
}
