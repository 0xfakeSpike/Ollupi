package cron

import (
	"context"
	"fmt"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/tools"
)

const (
	ProviderName  = "cron"
	ToolCreateJob = "create_scheduled_job"
	ToolDeleteJob = "delete_scheduled_job"
	ToolPauseJob  = "pause_scheduled_job"
	ToolListJobs  = "list_scheduled_jobs"
)

func (j *Jobs) Name() (string, string) {
	return ProviderName, "Scheduled tasks: messages sent to this conversation later, on a cron schedule."
}

func (j *Jobs) ToolList(context.Context) ([]*tools.Spec, error) {
	byID := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"id": map[string]any{"type": "string"}},
		"required":             []string{"id"},
		"additionalProperties": false,
	}
	return []*tools.Spec{
		{
			Name:        ToolCreateJob,
			Annotations: &tools.Annotations{DestructiveHint: tools.Hint(false), OpenWorldHint: tools.Hint(false)},
			Description: fmt.Sprintf("Schedule a task in this conversation: at each time the cron schedule names (timezone %s, now %s), "+
				"prompt is sent here as a new message and you work on it with this conversation's history, project, and settings. "+
				"If a turn is still running then, the message is sent when it ends.",
				j.location, time.Now().In(j.location).Format("2006-01-02 15:04 -07:00")),
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":     map[string]any{"type": "string", "pattern": idPattern.String(), "description": "Unique short task id, e.g. daily-market-report."},
					"spec":   map[string]any{"type": "string", "description": "5-field cron expression (minute hour day-of-month month day-of-week), or @hourly/@daily/@weekly/@monthly/@yearly."},
					"prompt": map[string]any{"type": "string", "minLength": 1, "maxLength": maxPromptRunes, "description": "The message sent at each run, as the user would write it."},
				},
				"required":             []string{"id", "spec", "prompt"},
				"additionalProperties": false,
			},
		},
		{
			Name:        ToolPauseJob,
			Description: "Pause or resume a scheduled task of this conversation. A resumed task runs next at its next scheduled time.",
			Annotations: &tools.Annotations{DestructiveHint: tools.Hint(false), OpenWorldHint: tools.Hint(false)},
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":     map[string]any{"type": "string"},
					"paused": map[string]any{"type": "boolean", "description": "true to pause, false to resume."},
				},
				"required":             []string{"id", "paused"},
				"additionalProperties": false,
			},
		},
		{
			Name:        ToolDeleteJob,
			Description: "Delete a scheduled task of this conversation by id.",
			Annotations: &tools.Annotations{DestructiveHint: tools.Hint(true), OpenWorldHint: tools.Hint(false)},
			InputSchema: byID,
		},
		{
			Name:        ToolListJobs,
			Description: "List the scheduled tasks of this conversation with their next run time.",
			Annotations: &tools.Annotations{ReadOnlyHint: true, OpenWorldHint: tools.Hint(false)},
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		},
	}, nil
}

func (j *Jobs) Invoke(ctx context.Context, tool string, args map[string]any) (tools.Result, error) {
	caller := tools.CallerOf(ctx)
	id, _ := args["id"].(string)
	switch tool {
	case ToolCreateJob:
		if caller.ThreadID == "" {
			return tools.Result{}, fmt.Errorf("cron: a task is created in a conversation")
		}
		if caller.Subagent {
			return tools.Result{}, fmt.Errorf("cron: a subagent cannot schedule tasks; report what should be scheduled instead")
		}
		spec, _ := args["spec"].(string)
		prompt, _ := args["prompt"].(string)
		job, err := j.Create(caller.ThreadID, id, spec, prompt)
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Result{Output: job}, nil
	case ToolPauseJob:
		paused, _ := args["paused"].(bool)
		job, err := j.SetPaused(id, caller.ThreadID, paused)
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Result{Output: job}, nil
	case ToolDeleteJob:
		if err := j.Delete(id, caller.ThreadID); err != nil {
			return tools.Result{}, err
		}
		return tools.Result{Output: map[string]any{"deleted": id}}, nil
	case ToolListJobs:
		jobs, err := j.db.CronJobs(caller.ThreadID)
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Result{Output: map[string]any{"jobs": jobs}}, nil
	default:
		return tools.Result{}, fmt.Errorf("cron: unknown tool %q", tool)
	}
}
