// Package cron runs scheduled jobs (cron jobs). It stores the jobs,
// fires them at their scheduled times, and exposes tools to manage them.
//
// The scheduler does not know the agent: when a job fires it calls the
// Trigger supplied by the composition root, which submits the job's prompt as
// a new thread. Creating and deleting jobs are not read-only, so
// under the on-request and untrusted approval policies they need approval.
package cron

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/0xfakeSpike/ollupi/execution"
	"github.com/0xfakeSpike/ollupi/tools"
	robfig "github.com/robfig/cron/v3"
)

const (
	ProviderName  = "cron"
	ToolCreateJob = "create_scheduled_job"
	ToolDeleteJob = "delete_scheduled_job"
	ToolListJobs  = "list_scheduled_jobs"

	maxJobs        = 64
	maxPromptRunes = 4000
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Job is one scheduled prompt.
type Job struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspaceId,omitempty"`
	Spec        string    `json:"spec"`
	Prompt      string    `json:"prompt"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Trigger runs a fired job. It must not block for the job's whole run.
type Trigger func(job Job) error

// Store persists jobs. Implemented by *storage.DB.
type Store interface {
	JobsSelectAll() ([]Job, error)
	JobInsert(job Job) error
	JobDelete(id string) (bool, error)
}

type Scheduler struct {
	store    Store
	location *time.Location
	parser   robfig.Parser
	cron     *robfig.Cron

	// opMu serializes Create and Delete end to end (validation, storage,
	// and the in-memory schedule), so they cannot interleave.
	opMu sync.Mutex
	// mu guards the maps and trigger for the cron goroutine.
	mu                sync.Mutex
	jobs              map[string]Job
	entries           map[string]robfig.EntryID
	trigger           Trigger
	validateWorkspace func(string) error
}

// New loads persisted jobs. Jobs do not fire until Start is called.
func New(store Store, location *time.Location) (*Scheduler, error) {
	parser := robfig.NewParser(robfig.Minute | robfig.Hour | robfig.Dom | robfig.Month | robfig.Dow | robfig.Descriptor)
	s := &Scheduler{
		store:    store,
		location: location,
		parser:   parser,
		cron:     robfig.New(robfig.WithParser(parser), robfig.WithLocation(location)),
		jobs:     make(map[string]Job),
		entries:  make(map[string]robfig.EntryID),
	}
	jobs, err := store.JobsSelectAll()
	if err != nil {
		return nil, fmt.Errorf("scheduler: load jobs: %w", err)
	}
	for _, job := range jobs {
		if err := s.schedule(job); err != nil {
			return nil, fmt.Errorf("scheduler: restore job %q: %w", job.ID, err)
		}
	}
	return s, nil
}

// RunWith runs job id now, outside its schedule, through run.
func (s *Scheduler) RunWith(id string, run Trigger) error {
	s.mu.Lock()
	job, ok := s.jobs[id]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("scheduler: job %q not found", id)
	}
	return run(job)
}

// Start begins firing jobs through trigger.
func (s *Scheduler) Start(trigger Trigger) {
	s.mu.Lock()
	s.trigger = trigger
	s.mu.Unlock()
	s.cron.Start()
}

// Stop stops firing and waits for running trigger calls to return.
func (s *Scheduler) Stop() {
	<-s.cron.Stop().Done()
}

func (s *Scheduler) schedule(job Job) error {
	entry, err := s.cron.AddFunc(job.Spec, func() { s.fire(job.ID) })
	if err != nil {
		return fmt.Errorf("invalid cron spec %q: %w", job.Spec, err)
	}
	s.mu.Lock()
	s.jobs[job.ID] = job
	s.entries[job.ID] = entry
	s.mu.Unlock()
	return nil
}

func (s *Scheduler) fire(id string) {
	s.mu.Lock()
	job, ok := s.jobs[id]
	trigger := s.trigger
	s.mu.Unlock()
	if !ok {
		return
	}
	if err := trigger(job); err != nil {
		log.Printf("scheduler: job %s failed to start: %v", id, err)
		return
	}
	log.Printf("scheduler: job %s started", id)
}

// validateSpec accepts a 5-field cron expression or one of the calendar
// descriptors. "@every" and "CRON_TZ=" are rejected: jobs run at calendar
// times in the configured timezone, at most once a minute.
func (s *Scheduler) validateSpec(spec string) error {
	if strings.HasPrefix(spec, "CRON_TZ=") || strings.HasPrefix(spec, "TZ=") {
		return fmt.Errorf("scheduler: timezone prefixes are not allowed; jobs use %s", s.location)
	}
	if strings.HasPrefix(spec, "@") {
		switch spec {
		case "@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly":
		default:
			return fmt.Errorf("scheduler: unsupported descriptor %q", spec)
		}
	} else if len(strings.Fields(spec)) != 5 {
		return fmt.Errorf("scheduler: cron spec %q must have 5 fields (minute hour day-of-month month day-of-week)", spec)
	}
	if _, err := s.parser.Parse(spec); err != nil {
		return fmt.Errorf("scheduler: invalid cron spec %q: %w", spec, err)
	}
	return nil
}

// Create validates, persists, and schedules a new job.
func (s *Scheduler) Create(id, spec, prompt string) (Job, error) {
	return s.CreateInWorkspace(id, spec, prompt, "")
}

// SetWorkspaceValidator is configured before the scheduler is made available.
func (s *Scheduler) SetWorkspaceValidator(validate func(string) error) {
	s.validateWorkspace = validate
}

func (s *Scheduler) CreateInWorkspace(id, spec, prompt, workspaceID string) (Job, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if workspaceID != "" {
		if s.validateWorkspace == nil {
			return Job{}, fmt.Errorf("scheduler: project workspaces are unavailable")
		}
		if err := s.validateWorkspace(workspaceID); err != nil {
			return Job{}, err
		}
	}
	id, spec, prompt = strings.TrimSpace(id), strings.TrimSpace(spec), strings.TrimSpace(prompt)
	if !idPattern.MatchString(id) {
		return Job{}, fmt.Errorf("scheduler: id %q must match %s", id, idPattern)
	}
	if err := s.validateSpec(spec); err != nil {
		return Job{}, err
	}
	if prompt == "" || len([]rune(prompt)) > maxPromptRunes {
		return Job{}, fmt.Errorf("scheduler: prompt must be 1 to %d characters", maxPromptRunes)
	}
	s.mu.Lock()
	_, exists := s.jobs[id]
	count := len(s.jobs)
	s.mu.Unlock()
	if exists {
		return Job{}, fmt.Errorf("scheduler: job %q already exists", id)
	}
	if count >= maxJobs {
		return Job{}, fmt.Errorf("scheduler: at most %d jobs are allowed", maxJobs)
	}
	job := Job{ID: id, WorkspaceID: workspaceID, Spec: spec, Prompt: prompt, CreatedAt: time.Now().UTC()}
	if err := s.store.JobInsert(job); err != nil {
		return Job{}, fmt.Errorf("scheduler: persist job %q: %w", id, err)
	}
	if err := s.schedule(job); err != nil {
		return Job{}, err
	}
	return job, nil
}

// Delete unschedules and removes a job. A firing already in progress when
// Delete is called may still start its run.
func (s *Scheduler) Delete(id string) error { return s.delete(id, nil) }

// DeleteInWorkspace is for agent tools: ownership is checked while the same
// operation lock protects lookup, deletion, and schedule removal. A concurrent
// delete/recreate cannot replace the job between authorization and mutation.
func (s *Scheduler) DeleteInWorkspace(id, workspaceID string) error {
	return s.delete(id, &workspaceID)
}

func (s *Scheduler) delete(id string, workspaceID *string) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	id = strings.TrimSpace(id)
	if workspaceID != nil {
		s.mu.Lock()
		job, exists := s.jobs[id]
		s.mu.Unlock()
		if !exists || job.WorkspaceID != *workspaceID {
			return fmt.Errorf("scheduler: job %q not found in this workspace", id)
		}
	}
	existed, err := s.store.JobDelete(id)
	if err != nil {
		return fmt.Errorf("scheduler: delete job %q: %w", id, err)
	}
	if !existed {
		return fmt.Errorf("scheduler: job %q not found", id)
	}
	s.mu.Lock()
	entry := s.entries[id]
	delete(s.jobs, id)
	delete(s.entries, id)
	s.mu.Unlock()
	s.cron.Remove(entry)
	return nil
}

// JobStatus is a job with its next fire time.
type JobStatus struct {
	Job
	Next time.Time `json:"next"`
}

// List returns every job with its next fire time, ordered by id. Next is
// computed from the job's schedule; it matches the cron entry once the
// scheduler is started.
func (s *Scheduler) List() []JobStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().In(s.location)
	out := make([]JobStatus, 0, len(s.jobs))
	for id, job := range s.jobs {
		out = append(out, JobStatus{Job: job, Next: s.cron.Entry(s.entries[id]).Schedule.Next(now)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ListInWorkspace limits agent tools to their conversation's jobs. General
// conversations see only general jobs; the management gateway still uses List.
func (s *Scheduler) ListInWorkspace(workspaceID string) []JobStatus {
	out := make([]JobStatus, 0)
	for _, job := range s.List() {
		if job.WorkspaceID == workspaceID {
			out = append(out, job)
		}
	}
	return out
}

// Provider exposes job management as tools.
type Provider struct {
	scheduler *Scheduler
}

func NewProvider(s *Scheduler) *Provider {
	return &Provider{scheduler: s}
}

func (p *Provider) Name() (string, string) {
	return ProviderName, "Scheduled jobs: run a prompt in a new thread on a cron schedule."
}

// ToolList annotates the job tools. Creating or deleting a job changes what
// the assistant does unattended later, so neither is read-only.
func (p *Provider) ToolList(context.Context) ([]*tools.Spec, error) {
	return []*tools.Spec{
		{
			Name:        ToolCreateJob,
			Annotations: &tools.Annotations{DestructiveHint: tools.Hint(false), OpenWorldHint: tools.Hint(false)},
			Description: fmt.Sprintf("Schedule a prompt to run in a new conversation thread on a cron schedule (timezone %s, now %s). "+
				"The prompt must be self-contained: it runs later without this conversation.",
				p.scheduler.location, time.Now().In(p.scheduler.location).Format("2006-01-02 15:04 -07:00")),
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":     map[string]any{"type": "string", "pattern": idPattern.String(), "description": "Unique short job id, e.g. daily-market-report."},
					"spec":   map[string]any{"type": "string", "description": "5-field cron expression (minute hour day-of-month month day-of-week), or @hourly/@daily/@weekly/@monthly/@yearly."},
					"prompt": map[string]any{"type": "string", "minLength": 1, "maxLength": maxPromptRunes, "description": "Complete instructions for each run."},
				},
				"required":             []string{"id", "spec", "prompt"},
				"additionalProperties": false,
			},
		},
		{
			Name:        ToolDeleteJob,
			Description: "Delete a scheduled job in this workspace by id.",
			Annotations: &tools.Annotations{DestructiveHint: tools.Hint(true), OpenWorldHint: tools.Hint(false)},
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"id": map[string]any{"type": "string"}},
				"required":             []string{"id"},
				"additionalProperties": false,
			},
		},
		{
			Name:        ToolListJobs,
			Description: "List scheduled jobs in this workspace with their next run time.",
			Annotations: &tools.Annotations{ReadOnlyHint: true, OpenWorldHint: tools.Hint(false)},
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		},
	}, nil
}

func (p *Provider) Invoke(ctx context.Context, tool string, args map[string]any) (tools.Result, error) {
	switch tool {
	case ToolCreateJob:
		id, _ := args["id"].(string)
		spec, _ := args["spec"].(string)
		prompt, _ := args["prompt"].(string)
		job, err := p.scheduler.CreateInWorkspace(id, spec, prompt, execution.WorkspaceID(ctx))
		if err != nil {
			return tools.Result{Err: err}, err
		}
		return tools.Result{Output: map[string]any{"created": job.ID, "spec": job.Spec}}, nil
	case ToolDeleteJob:
		id, _ := args["id"].(string)
		if err := p.scheduler.DeleteInWorkspace(id, execution.WorkspaceID(ctx)); err != nil {
			return tools.Result{Err: err}, err
		}
		return tools.Result{Output: map[string]any{"deleted": id}}, nil
	case ToolListJobs:
		return tools.Result{Output: map[string]any{"jobs": p.scheduler.ListInWorkspace(execution.WorkspaceID(ctx))}}, nil
	default:
		err := fmt.Errorf("scheduler: unknown tool %q", tool)
		return tools.Result{Err: err}, err
	}
}
