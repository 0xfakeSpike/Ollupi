package cron

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/repo"
	robfig "github.com/robfig/cron/v3"
)

const (
	maxJobs        = 64
	maxPromptRunes = 4000
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type Jobs struct {
	db       *repo.DB
	location *time.Location
	parser   robfig.Parser
}

func New(db *repo.DB, location *time.Location) *Jobs {
	return &Jobs{
		db:       db,
		location: location,
		parser:   robfig.NewParser(robfig.Minute | robfig.Hour | robfig.Dom | robfig.Month | robfig.Dow | robfig.Descriptor),
	}
}

func (j *Jobs) schedule(spec string) (robfig.Schedule, error) {
	if strings.HasPrefix(spec, "CRON_TZ=") || strings.HasPrefix(spec, "TZ=") {
		return nil, fmt.Errorf("cron: time-zone prefixes are not allowed; tasks use %s", j.location)
	}
	if strings.HasPrefix(spec, "@") {
		switch spec {
		case "@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly":
		default:
			return nil, fmt.Errorf("cron: unsupported descriptor %q", spec)
		}
	} else if len(strings.Fields(spec)) != 5 {
		return nil, fmt.Errorf("cron: cron spec %q must have 5 fields (minute hour day-of-month month day-of-week)", spec)
	}
	s, err := j.parser.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("cron: invalid cron spec %q: %w", spec, err)
	}
	return s, nil
}

func (j *Jobs) next(spec string, after time.Time) (time.Time, error) {
	s, err := j.schedule(spec)
	if err != nil {
		return time.Time{}, err
	}
	return s.Next(after.In(j.location)).UTC(), nil
}

func (j *Jobs) Create(threadID, id, spec, prompt string) (repo.CronJobRow, error) {
	id, spec, prompt = strings.TrimSpace(id), strings.TrimSpace(spec), strings.TrimSpace(prompt)
	if threadID == "" {
		return repo.CronJobRow{}, fmt.Errorf("cron: a task belongs to a conversation")
	}
	if !idPattern.MatchString(id) {
		return repo.CronJobRow{}, fmt.Errorf("cron: id %q must match %s", id, idPattern)
	}
	if prompt == "" || len([]rune(prompt)) > maxPromptRunes {
		return repo.CronJobRow{}, fmt.Errorf("cron: prompt must be 1 to %d characters", maxPromptRunes)
	}
	now := time.Now().UTC()
	next, err := j.next(spec, now)
	if err != nil {
		return repo.CronJobRow{}, err
	}
	job := repo.CronJobRow{ID: id, ThreadID: threadID, Spec: spec, Prompt: prompt, NextRunAt: next, CreatedAt: now}
	if err := j.db.CreateCronJob(job, maxJobs); err != nil {
		return repo.CronJobRow{}, err
	}
	return job, nil
}

func (j *Jobs) Delete(id, threadID string) error {
	_, err := j.change(id, threadID, func(repo.CronJobRow) (*repo.CronJobRow, error) { return nil, nil })
	return err
}

func (j *Jobs) SetPaused(id, threadID string, paused bool) (repo.CronJobRow, error) {
	return j.change(id, threadID, func(job repo.CronJobRow) (*repo.CronJobRow, error) {
		job.Paused = paused
		if !paused {
			next, err := j.next(job.Spec, time.Now().UTC())
			if err != nil {
				return nil, err
			}
			job.NextRunAt = next
		}
		return &job, nil
	})
}

func (j *Jobs) Sent(job repo.CronJobRow, now time.Time) error {
	next, err := j.next(job.Spec, now)
	if err != nil {
		return err
	}
	return j.db.SetCronJobNextRun(job.ID, job.NextRunAt, next)
}

func (j *Jobs) change(id, threadID string, fn func(repo.CronJobRow) (*repo.CronJobRow, error)) (repo.CronJobRow, error) {
	row, err := j.db.UpdateCronJob(strings.TrimSpace(id), threadID, fn)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.CronJobRow{}, fmt.Errorf("cron: task %q not found", id)
	}
	return row, err
}
