// Package scheduler coordinates periodic collectors with a fixed worker pool.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Job struct {
	Name     string
	Interval time.Duration
	Timeout  time.Duration
	Collect  func(context.Context) error
}

type State struct {
	Name      string `json:"name"`
	Running   bool   `json:"running"`
	Runs      uint64 `json:"runs"`
	Skipped   uint64 `json:"skipped"`
	LastError string `json:"last_error,omitempty"`
}

type scheduledJob struct {
	job   Job
	state State
	next  time.Time
}

type Scheduler struct {
	mu          sync.Mutex
	jobs        []*scheduledJob
	concurrency int
	onError     func(string, error)
	running     bool
}

// New uses at least one worker. Add validates each job before Run starts.
func New(concurrency int, onError func(string, error)) *Scheduler {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Scheduler{concurrency: concurrency, onError: onError}
}

func (s *Scheduler) Add(job Job) error {
	if job.Name == "" || job.Interval <= 0 || job.Timeout <= 0 || job.Collect == nil {
		return errors.New("collector needs a name, positive interval/timeout and collect function")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return errors.New("cannot add collector while scheduler is running")
	}
	for _, existing := range s.jobs {
		if existing.job.Name == job.Name {
			return fmt.Errorf("duplicate collector name %q", job.Name)
		}
	}
	s.jobs = append(s.jobs, &scheduledJob{job: job, state: State{Name: job.Name}})
	return nil
}

// Run starts jobs immediately, then at their fixed intervals. A queued or running
// job never overlaps itself; missed runs are skipped instead of accumulating.
// Cancellation is a successful shutdown. Collect must respect its context:
// Go cannot forcibly terminate a collector that ignores cancellation.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("scheduler is already running")
	}
	s.running = true
	now := time.Now()
	jobs := append([]*scheduledJob(nil), s.jobs...)
	for _, job := range jobs {
		job.next = now
	}
	s.mu.Unlock()
	work := make(chan *scheduledJob, s.concurrency)
	var workers sync.WaitGroup
	for range s.concurrency {
		workers.Go(func() { s.worker(ctx, work) })
	}
	defer func() {
		close(work)
		workers.Wait()
		s.mu.Lock()
		s.running = false
		for _, job := range jobs {
			job.state.Running = false
		}
		s.mu.Unlock()
	}()
	if len(jobs) == 0 {
		<-ctx.Done()
		return nil
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		now = time.Now()
		s.mu.Lock()
		var next time.Time
		for _, job := range jobs {
			if !job.next.After(now) {
				periods := uint64(now.Sub(job.next)/job.job.Interval) + 1
				job.next = job.next.Add(time.Duration(periods) * job.job.Interval)
				if job.state.Running {
					job.state.Skipped += periods
				} else {
					job.state.Skipped += periods - 1
					job.state.Running = true
					select {
					case work <- job:
					default:
						job.state.Running = false
						job.state.Skipped++
					}
				}
			}
			if next.IsZero() || job.next.Before(next) {
				next = job.next
			}
		}
		s.mu.Unlock()
		delay := time.Until(next)
		if delay < 0 {
			delay = 0
		}
		timer.Reset(delay)
	}
}

func (s *Scheduler) worker(ctx context.Context, work <-chan *scheduledJob) {
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case job, ok := <-work:
			if !ok {
				return
			}
			if ctx.Err() != nil {
				return
			}
			jobCtx, cancel := context.WithTimeout(ctx, job.job.Timeout)
			err := collect(jobCtx, job.job.Collect)
			if jobCtx.Err() != nil {
				err = errors.Join(err, jobCtx.Err())
			}
			cancel()
			s.mu.Lock()
			job.state.Runs++
			job.state.Running = false
			if err != nil {
				message := err.Error()
				if len(message) > 1024 {
					// Clone releases the potentially large source error buffer; slicing
					// alone would retain it even though the visible summary is short.
					message = strings.Clone(message[:1024])
				}
				job.state.LastError = message
			} else {
				job.state.LastError = ""
			}
			s.mu.Unlock()
			if err != nil && ctx.Err() == nil && s.onError != nil {
				s.onError(job.job.Name, err)
			}
		}
	}
}

func collect(ctx context.Context, fn func(context.Context) error) (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("collector panic: %v", value)
		}
	}()
	return fn(ctx)
}

func (s *Scheduler) Stats() []State {
	s.mu.Lock()
	defer s.mu.Unlock()
	states := make([]State, len(s.jobs))
	for i, job := range s.jobs {
		states[i] = job.state
	}
	return states
}
