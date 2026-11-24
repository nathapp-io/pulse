// Package schedule runs one worker goroutine per target.
//
// Each worker waits a jittered interval, takes a slot from a global semaphore,
// runs the probe, and hands the result to the caller. A worker never runs two
// probes concurrently, so a slow target cannot pile up goroutines.
package schedule

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"pulse/internal/probe"
)

// Target is one scheduled probe.
type Target struct {
	Name     string
	Prober   probe.Prober
	Interval time.Duration
}

// Handler receives every probe result, in completion order.
type Handler func(probe.Result)

// Runner owns the workers and the concurrency limit.
type Runner struct {
	sem chan struct{}

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New returns a runner that allows at most maxConcurrent probes in flight.
func New(maxConcurrent int) *Runner {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Runner{sem: make(chan struct{}, maxConcurrent)}
}

// Start launches one worker per target. It is safe to call Stop then Start again
// with a new target set, which is how a config reload is applied.
func (r *Runner) Start(parent context.Context, targets []Target, handle Handler) {
	ctx, cancel := context.WithCancel(parent)

	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()

	for _, t := range targets {
		r.wg.Add(1)
		go r.worker(ctx, t, handle)
	}
}

// Stop cancels every worker and waits for them to return.
func (r *Runner) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	r.wg.Wait()
}

func (r *Runner) worker(ctx context.Context, t Target, handle Handler) {
	defer r.wg.Done()

	interval := t.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	// Stagger the first probe so 100 targets do not all fire on the same tick.
	delay := jitter(interval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		select {
		case r.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}

		res := t.Prober.Probe(ctx)
		<-r.sem

		if ctx.Err() != nil {
			return
		}
		handle(res)

		delay = interval + jitter(interval/10)
	}
}

func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(max)))
}
