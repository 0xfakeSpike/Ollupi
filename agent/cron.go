package agent

import (
	"log"
	"time"

	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

func (a *Agent) keepTime() {
	defer a.loops.Done()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		a.mu.Lock()
		a.sendDueLocked("")
		a.mu.Unlock()
		select {
		case <-a.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *Agent) sendDueLocked(threadID string) {
	if a.ctx.Err() != nil {
		return
	}
	now := time.Now().UTC()
	due, err := a.DB.DueCronJobs(now)
	if err != nil {
		log.Printf("agent: scheduled tasks: %v", err)
		return
	}
	for _, job := range due {
		if threadID != "" && job.ThreadID != threadID {
			continue
		}
		view, ok := a.ReadThread(job.ThreadID)
		if !ok {
			continue
		}
		if _, active := view.ActiveTurn(); active {
			continue
		}
		err := a.sendJobLocked(view, job)
		if err != nil {
			log.Printf("agent: scheduled task %s could not start in thread %s: %v", job.ID, job.ThreadID, err)
		}
		if err := a.cron.Sent(job, now); err != nil {
			log.Printf("agent: scheduled task %s: record its run: %v", job.ID, err)
		}
	}
}

func (a *Agent) sendJobLocked(view repo.ThreadView, job repo.CronJobRow) error {
	text, err := checkMessage(job.Prompt)
	if err != nil {
		return err
	}
	model, err := a.conversationModel(view, "")
	if err != nil {
		return err
	}
	message := repo.NewUserMessage(uuid.NewString(), "", text, time.Now().UTC())
	message.CronJobID = job.ID
	_, err = a.beginTurnLocked(view, message, model, a.threadPolicy(view.Thread))
	return err
}
