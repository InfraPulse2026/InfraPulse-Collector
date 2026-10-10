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
	job          Job
	state        State
	next         time.Time
	paused       bool
	resumeOnDone bool
	cancel       context.CancelFunc
	done         chan struct{}
	ctx          context.Context
}

type Scheduler struct {
	mu          sync.Mutex
	jobs        []*scheduledJob
	concurrency int
	onError     func(string, error)
	running     bool
	changed     chan struct{}
	change      *Change
}

// New uses at least one worker. Add validates each job before Run starts.
func New(concurrency int, onError func(string, error)) *Scheduler {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Scheduler{concurrency: concurrency, onError: onError, changed: make(chan struct{}, 1)}
}

func (s *Scheduler) Add(job Job) error {
	if job.Name == "" || job.Interval <= 0 || job.Timeout <= 0 || job.Collect == nil {
		return errors.New("collector needs a name, positive interval/timeout and collect function")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running || s.change != nil {
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
	for _, job := range s.jobs {
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
		for _, job := range s.jobs {
			s.completeLocked(job)
		}
		s.mu.Unlock()
	}()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.changed:
		case <-timer.C:
		}
		now = time.Now()
		s.mu.Lock()
		var next time.Time
		for _, job := range s.jobs {
			if job.paused {
				continue
			}
			if !job.next.After(now) {
				periods := uint64(now.Sub(job.next)/job.job.Interval) + 1
				job.next = job.next.Add(time.Duration(periods) * job.job.Interval)
				if job.state.Running {
					job.state.Skipped += periods
				} else {
					job.state.Skipped += periods - 1
					job.state.Running = true
					job.ctx, job.cancel = context.WithCancel(ctx)
					job.done = make(chan struct{})
					select {
					case work <- job:
					default:
						s.completeLocked(job)
						job.state.Skipped++
					}
				}
			}
			if next.IsZero() || job.next.Before(next) {
				next = job.next
			}
		}
		s.mu.Unlock()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if next.IsZero() {
			continue
		}
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
			s.mu.Lock()
			if job.paused || job.ctx.Err() != nil {
				s.completeLocked(job)
				s.mu.Unlock()
				continue
			}
			base := job.ctx
			s.mu.Unlock()
			jobCtx, cancel := context.WithTimeout(base, job.job.Timeout)
			err := collect(jobCtx, job.job.Collect)
			if jobCtx.Err() != nil {
				err = errors.Join(err, jobCtx.Err())
			}
			cancel()
			s.mu.Lock()
			job.state.Runs++
			transitionCancelled := job.paused || job.resumeOnDone
			s.completeLocked(job)
			if err != nil && !transitionCancelled {
				message := err.Error()
				if len(message) > 1024 {
					// Clone releases the potentially large source error buffer; slicing
					// alone would retain it even though the visible summary is short.
					message = strings.Clone(message[:1024])
				}
				job.state.LastError = message
			} else if !transitionCancelled {
				job.state.LastError = ""
			}
			s.mu.Unlock()
			if err != nil && ctx.Err() == nil && !transitionCancelled && s.onError != nil {
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
