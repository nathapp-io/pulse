// Package push ships probe results from a prober to the viewer.
//
// The queue is a bounded FIFO of results, so a viewer outage costs the oldest
// samples rather than unbounded memory. Delivery is at-least-once: a batch stays
// queued until the viewer accepts it, and the viewer's ingest is idempotent on
// (prober, target, at), so a retry stores nothing twice.
package push

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"time"

	"pulse/internal/model"
)

// Observer receives queue and delivery counters. metrics.Metrics satisfies it.
type Observer interface {
	PushQueued(n int)
	PushDropped()
	PushSucceeded(t time.Time)
	PushFailed()
}

// Config is the client's static configuration.
type Config struct {
	// URL is the full ingest endpoint, e.g. https://pulse.example.com/api/v1/ingest.
	URL string
	// Token is the bearer token. Empty disables pushing (callers decide).
	Token string
	// Interval is the batch cadence. State changes flush immediately.
	Interval time.Duration
	// Timeout bounds one HTTP request.
	Timeout time.Duration
	// QueueSize bounds the number of pending results.
	QueueSize int
	// MinBackoff and MaxBackoff bound retry delay. Defaults 1s / 60s.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// MaxBatch caps results per request.
	MaxBatch int
	// InsecureSkipVerify disables TLS verification (self-signed viewers).
	InsecureSkipVerify bool

	ProberName    string
	ProberVersion string
	Labels        map[string]string
	// Inventory snapshots the accepted targets and config hash together.
	Inventory func() ([]model.TargetSpec, string)
}

type entry struct {
	seq uint64
	r   model.Result
}

// Client queues results and pushes them.
type Client struct {
	cfg Config
	obs Observer
	hc  *http.Client

	mu      sync.Mutex
	queue   []entry
	nextSeq uint64
	dropped uint64

	notify chan struct{}

	// Only the delivery loop takes inventory snapshots, including during its final flush.
	lastInventoryRevision int64

	// now is injectable for tests.
	now func() time.Time
}

// New validates the configuration and builds a client.
func New(cfg Config, obs Observer) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("viewer URL is required")
	}
	parsed, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("viewer URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("viewer URL must be http or https, got %q", parsed.Scheme)
	}
	if cfg.Token == "" {
		return nil, errors.New("viewer token is empty")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 10000
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 60 * time.Second
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 500
	}
	if obs == nil {
		obs = nopObserver{}
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in for self-signed viewers
	}

	return &Client{
		cfg:    cfg,
		obs:    obs,
		hc:     &http.Client{Timeout: cfg.Timeout, Transport: transport},
		notify: make(chan struct{}, 1),
		now:    time.Now,
	}, nil
}

// Enqueue adds a result, dropping the oldest when the queue is full: the newest
// samples are what the UI shows as current, so they are the ones worth keeping.
func (c *Client) Enqueue(r model.Result) {
	c.mu.Lock()
	c.nextSeq++
	c.queue = append(c.queue, entry{seq: c.nextSeq, r: r})
	droppedNow := 0
	for len(c.queue) > c.cfg.QueueSize {
		c.queue = c.queue[1:]
		c.dropped++
		droppedNow++
	}
	queued := len(c.queue)
	c.mu.Unlock()

	// Report outside the lock: observers touch Prometheus, not our state.
	for i := 0; i < droppedNow; i++ {
		c.obs.PushDropped()
	}
	c.obs.PushQueued(queued)
}

// Dropped reports how many results were discarded because the queue was full.
func (c *Client) Dropped() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// Queued reports the current queue depth.
func (c *Client) Queued() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue)
}

// FlushNow asks the run loop to push as soon as possible. Safe to call from any
// goroutine; coalesces bursts (a reload or a flap storm) into one early flush.
func (c *Client) FlushNow() {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// Run pushes until ctx is cancelled, then makes one final attempt so a clean
// shutdown does not throw away results the viewer never saw.
func (c *Client) Run(ctx context.Context) {
	timer := time.NewTimer(c.cfg.Interval)
	defer timer.Stop()

	backoff := time.Duration(0)

	for {
		select {
		case <-ctx.Done():
			c.finalFlush()
			return
		case <-c.notify:
		case <-timer.C:
		}

		err := c.flush(ctx)
		if err != nil {
			if ctx.Err() != nil {
				c.finalFlush()
				return
			}
			backoff = nextBackoff(backoff, c.cfg.MinBackoff, c.cfg.MaxBackoff)
			resetTimer(timer, backoff)
			continue
		}

		backoff = 0
		if c.Queued() > 0 {
			// A backlog larger than MaxBatch drains immediately instead of one
			// batch per interval.
			resetTimer(timer, 0)
			continue
		}
		resetTimer(timer, c.cfg.Interval)
	}
}

// finalFlush delivers what is queued, with its own deadline since the run
// context is already cancelled. It loops so a backlog larger than one batch is
// not silently abandoned at shutdown.
func (c *Client) finalFlush() {
	if c.Queued() == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
	defer cancel()
	for c.Queued() > 0 {
		if err := c.flush(ctx); err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// flush sends at most MaxBatch queued results. On success it removes exactly the
// entries that were sent; on failure they stay queued for the next attempt.
func (c *Client) flush(ctx context.Context) error {
	c.mu.Lock()
	n := len(c.queue)
	if n > c.cfg.MaxBatch {
		n = c.cfg.MaxBatch
	}
	if n == 0 && c.cfg.Inventory == nil {
		c.mu.Unlock()
		return nil
	}
	batch := make([]entry, n)
	copy(batch, c.queue[:n])
	c.mu.Unlock()

	payload := model.Batch{
		Prober: model.ProberInfo{
			Name:    c.cfg.ProberName,
			Version: c.cfg.ProberVersion,
			Labels:  c.cfg.Labels,
		},
		Results: make([]model.Result, 0, len(batch)),
	}
	if c.cfg.Inventory != nil {
		payload.Targets, payload.ConfigHash = c.cfg.Inventory()
		if payload.Targets != nil {
			// Stamp the declaration, not the queued measurements: after a reload
			// a fresh target list can travel with hours-old samples. Wall time
			// orders snapshots across restarts; the floor handles clock steps
			// and repeated timestamps while this sender is running.
			revision := c.now().UnixNano()
			if revision <= c.lastInventoryRevision {
				revision = c.lastInventoryRevision + 1
			}
			payload.InventoryRevision = revision
			c.lastInventoryRevision = revision
		}
	}
	for _, e := range batch {
		payload.Results = append(payload.Results, e.r)
	}

	if len(batch) == 0 && payload.Targets == nil {
		return nil
	}
	if err := c.post(ctx, payload); err != nil {
		c.obs.PushFailed()
		return err
	}

	// Remove what was sent. Entries added while the request was in flight have a
	// higher sequence number and are kept.
	if len(batch) == 0 {
		c.obs.PushSucceeded(c.now())
		return nil
	}
	lastSent := batch[len(batch)-1].seq
	c.mu.Lock()
	kept := c.queue[:0]
	for _, e := range c.queue {
		if e.seq > lastSent {
			kept = append(kept, e)
		}
	}
	c.queue = kept
	queued := len(c.queue)
	c.mu.Unlock()

	c.obs.PushQueued(queued)
	c.obs.PushSucceeded(c.now())
	return nil
}

func (c *Client) post(ctx context.Context, batch model.Batch) error {
	body, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("encode batch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("User-Agent", "pulse-prober/"+c.cfg.ProberVersion)

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("push: viewer returned %s", resp.Status)
	}
	return nil
}

// nextBackoff doubles the delay up to max, then adds up to 20% jitter so a fleet
// of probers does not retry in lockstep.
func nextBackoff(current, min, max time.Duration) time.Duration {
	next := current * 2
	if next < min {
		next = min
	}
	if next > max {
		next = max
	}
	if next <= 0 {
		return min
	}
	return next + time.Duration(rand.Int63n(int64(next)/5+1))
}

type nopObserver struct{}

func (nopObserver) PushQueued(int)          {}
func (nopObserver) PushDropped()            {}
func (nopObserver) PushSucceeded(time.Time) {}
func (nopObserver) PushFailed()             {}
