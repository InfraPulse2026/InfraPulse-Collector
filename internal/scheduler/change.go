package scheduler

import (
	"context"
	"errors"
	"time"
)

// Change replaces only selected jobs. Unchanged jobs retain their deadlines,
// counters and running operations. A single reservation serializes transitions.
// Apply drains removed jobs; replacement jobs remain paused until Commit.
type Change struct {
	scheduler                 *Scheduler
	old, next, removed, added []*scheduledJob
	state                     uint8 // 0 prepared, 1 applied, 2 finished
	committed                 bool
}

func validJob(job Job) bool {
	return job.Name != "" && job.Interval > 0 && job.Timeout > 0 && job.Collect != nil
}

func (s *Scheduler) signalLocked() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *Scheduler) completeLocked(job *scheduledJob) {
	if job.cancel != nil {
		job.cancel()
		job.cancel = nil
	}
	if job.done != nil {
		close(job.done)
		job.done = nil
	}
	job.state.Running = false
	if job.resumeOnDone {
		job.resumeOnDone, job.paused = false, false
		job.next = time.Now()
		s.signalLocked()
	}
}

func (s *Scheduler) PrepareChanges(removeNames []string, add []Job) (*Change, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.change != nil {
		return nil, errors.New("scheduler transition already pending")
	}
	remove := make(map[string]bool, len(removeNames))
	for _, name := range removeNames {
		if name == "" || remove[name] {
			return nil, errors.New("invalid scheduler removal")
		}
		remove[name] = true
	}
	change := &Change{scheduler: s, old: append([]*scheduledJob(nil), s.jobs...)}
	names := make(map[string]bool, len(s.jobs)+len(add))
	for _, job := range s.jobs {
		if remove[job.job.Name] {
			change.removed = append(change.removed, job)
			delete(remove, job.job.Name)
		} else {
			change.next = append(change.next, job)
			names[job.job.Name] = true
		}
	}
	if len(remove) != 0 {
		return nil, errors.New("scheduler removal does not exist")
	}
	for _, job := range add {
		if !validJob(job) || names[job.Name] {
			return nil, errors.New("invalid or duplicate scheduler replacement")
		}
		names[job.Name] = true
		prepared := &scheduledJob{job: job, state: State{Name: job.Name}, paused: true}
		change.added = append(change.added, prepared)
		change.next = append(change.next, prepared)
	}
	s.change = change
	return change, nil
}

func (c *Change) Apply(ctx context.Context) error {
	s := c.scheduler
	s.mu.Lock()
	if c.state != 0 || s.change != c {
		s.mu.Unlock()
		return errors.New("scheduler transition is not prepared")
	}
	if err := ctx.Err(); err != nil {
		c.restoreLocked()
		s.mu.Unlock()
		return err
	}
	var waiting []<-chan struct{}
	for _, job := range c.removed {
		job.paused = true
		job.resumeOnDone = false
		if job.cancel != nil {
			job.cancel()
		}
		if job.done != nil {
			waiting = append(waiting, job.done)
		}
	}
	s.signalLocked()
	s.mu.Unlock()
	for _, done := range waiting {
		select {
		case <-done:
		case <-ctx.Done():
			s.mu.Lock()
			c.restoreLocked()
			s.mu.Unlock()
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		c.restoreLocked()
		return err
	}
	if c.state != 0 || s.change != c {
		return errors.New("scheduler transition was closed")
	}
	s.jobs = c.next
	c.state = 1
	s.signalLocked()
	return nil
}

func (c *Change) Commit() error {
	s := c.scheduler
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.state == 2 {
		if c.committed {
			return nil
		}
		return errors.New("scheduler transition was rolled back")
	}
	if c.state != 1 || s.change != c {
		return errors.New("scheduler transition is not applied")
	}
	for _, job := range c.added {
		job.paused = false
		job.next = time.Now()
	}
	c.state, s.change = 2, nil
	c.committed = true
	s.signalLocked()
	return nil
}

// Rollback never schedules an old job while its canceled operation still runs.
// A context expiry leaves it paused until that operation completes; its worker
// then resumes the original job without allocating another cleanup goroutine.
func (c *Change) Rollback(ctx context.Context) error {
	s := c.scheduler
	s.mu.Lock()
	if c.state == 2 {
		s.mu.Unlock()
		return nil
	}
	var waiting []<-chan struct{}
	for _, job := range c.removed {
		if job.done != nil {
			waiting = append(waiting, job.done)
		}
	}
	c.restoreLocked()
	s.mu.Unlock()
	for _, done := range waiting {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (c *Change) restoreLocked() {
	s := c.scheduler
	if c.state == 2 {
		return
	}
	if c.state == 1 {
		s.jobs = c.old
	}
	for _, job := range c.removed {
		if job.state.Running {
			job.resumeOnDone = true
		} else {
			job.paused = false
			job.next = time.Now()
		}
	}
	c.state = 2
	if s.change == c {
		s.change = nil
	}
	s.signalLocked()
}

func (c *Change) Close() error {
	s := c.scheduler
	s.mu.Lock()
	c.restoreLocked()
	s.mu.Unlock()
	return nil
}
