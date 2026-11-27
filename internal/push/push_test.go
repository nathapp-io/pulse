package push

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"pulse/internal/model"
)

type fakeObserver struct {
	mu         sync.Mutex
	queued     int
	everQueued int
	dropped    int
	succeeded  int
	failed     int
}

func (f *fakeObserver) PushQueued(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued = n
	if n > f.everQueued {
		f.everQueued = n
	}
}
func (f *fakeObserver) PushDropped()            { f.mu.Lock(); f.dropped++; f.mu.Unlock() }
func (f *fakeObserver) PushSucceeded(time.Time) { f.mu.Lock(); f.succeeded++; f.mu.Unlock() }
func (f *fakeObserver) PushFailed()             { f.mu.Lock(); f.failed++; f.mu.Unlock() }

func (f *fakeObserver) snapshot() (queued, dropped, succeeded, failed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queued, f.dropped, f.succeeded, f.failed
}

// recorder is a viewer stand-in that records batches and can fail on demand.
type recorder struct {
	mu      sync.Mutex
	batches []model.Batch
	headers []string
	failFor int
	calls   int
}

func (r *recorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.calls++
		call := r.calls
		fail := r.failFor > 0 && call <= r.failFor
		r.mu.Unlock()

		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}

		var b model.Batch
		if err := json.NewDecoder(req.Body).Decode(&b); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.batches = append(r.batches, b)
		r.headers = append(r.headers, req.Header.Get("Authorization"))
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.batches)
}

func (r *recorder) all() []model.Batch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]model.Batch(nil), r.batches...)
}

func testResult(target string, at time.Time) model.Result {
	return model.Result{Target: target, At: at, OK: true, State: "UP", DurationMS: 5}
}

func newClient(t *testing.T, url string, obs Observer, mutate func(*Config)) *Client {
	t.Helper()
	cfg := Config{
		URL:           url,
		Token:         "secret",
		Interval:      50 * time.Millisecond,
		Timeout:       time.Second,
		QueueSize:     100,
		MinBackoff:    10 * time.Millisecond,
		MaxBackoff:    40 * time.Millisecond,
		ProberName:    "prober-a",
		ProberVersion: "test",
		Inventory: func() ([]model.TargetSpec, string) {
			return []model.TargetSpec{{Name: "api", Group: "web", Type: "http"}}, "hash"
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := New(cfg, obs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestFlushSendsBatch(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	obs := &fakeObserver{}
	c := newClient(t, srv.URL+"/api/v1/ingest", obs, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	now := time.Now().UTC().Truncate(time.Second)
	c.Enqueue(testResult("api", now))
	c.Enqueue(testResult("api", now.Add(time.Second)))

	waitFor(t, "a delivered batch", func() bool { return rec.count() >= 1 })

	batch := rec.all()[0]
	if batch.Prober.Name != "prober-a" || batch.Prober.Version != "test" {
		t.Fatalf("prober = %+v", batch.Prober)
	}
	if len(batch.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(batch.Results))
	}
	if len(batch.Targets) != 1 || batch.Targets[0].Name != "api" {
		t.Fatalf("targets = %+v, want the full target list", batch.Targets)
	}

	rec.mu.Lock()
	auth := rec.headers[0]
	rec.mu.Unlock()
	if auth != "Bearer secret" {
		t.Fatalf("Authorization = %q, want the bearer token", auth)
	}

	waitFor(t, "an empty queue", func() bool { return c.Queued() == 0 })
}

func TestInventoryRevisionFollowsSnapshotNotMeasurementTime(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	hash := "old-config"
	inventory := []model.TargetSpec{{Name: "old"}}
	c := newClient(t, srv.URL, nil, func(cfg *Config) { cfg.Inventory = func() ([]model.TargetSpec, string) { return inventory, hash } })
	now := time.Now().UTC()
	c.now = func() time.Time { return now }
	flush := func(at time.Time) {
		t.Helper()
		c.Enqueue(testResult("old", at))
		if err := c.flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	flush(now)
	// The newly loaded inventory is attached to older backlog while wall time
	// moves backwards. It must still supersede the preceding declaration.
	hash = "new-config"
	inventory = []model.TargetSpec{{Name: "new"}}
	now = now.Add(-time.Second)
	flush(now.Add(-time.Hour))
	inventory = nil
	flush(now)
	got := rec.all()
	if len(got) != 3 {
		t.Fatalf("got %d batches, want 3", len(got))
	}
	if got[0].ConfigHash != "old-config" || got[1].ConfigHash != "new-config" {
		t.Fatalf("configuration hashes: %q, %q", got[0].ConfigHash, got[1].ConfigHash)
	}
	if got[0].InventoryRevision <= 0 || got[1].InventoryRevision <= got[0].InventoryRevision || got[1].Targets[0].Name != "new" || got[2].InventoryRevision != 0 {
		t.Errorf("wrong inventory revisions: %d %d %d", got[0].InventoryRevision, got[1].InventoryRevision, got[2].InventoryRevision)
	}
}

func TestFlushNowBeatsTheInterval(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	// A one-hour interval: only FlushNow can make this test pass quickly.
	c := newClient(t, srv.URL, nil, func(cfg *Config) { cfg.Interval = time.Hour })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	c.Enqueue(testResult("api", time.Now().UTC()))
	c.FlushNow()

	waitFor(t, "an immediate flush", func() bool { return rec.count() >= 1 })
}

func TestQueueIsBoundedAndDropsOldest(t *testing.T) {
	obs := &fakeObserver{}
	c := newClient(t, "http://127.0.0.1:1", obs, func(cfg *Config) { cfg.QueueSize = 2 })

	base := time.Now().UTC()
	c.Enqueue(testResult("api", base))
	c.Enqueue(testResult("api", base.Add(time.Second)))
	c.Enqueue(testResult("api", base.Add(2*time.Second))) // evicts the first

	if got := c.Queued(); got != 2 {
		t.Fatalf("queued = %d, want 2", got)
	}
	if got := c.Dropped(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}

	_, dropped, _, _ := obs.snapshot()
	if dropped != 1 {
		t.Fatalf("observer dropped = %d, want 1", dropped)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.queue[0].r.At.Equal(base.Add(time.Second)) {
		t.Fatalf("oldest kept at %s, want the newest two (dropped the oldest)", c.queue[0].r.At)
	}
}

func TestRetriesUntilAccepted(t *testing.T) {
	rec := &recorder{failFor: 2}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	obs := &fakeObserver{}
	c := newClient(t, srv.URL, obs, func(cfg *Config) { cfg.Interval = 20 * time.Millisecond })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	c.Enqueue(testResult("api", time.Now().UTC()))

	// The recorder sees the batch before the client receives its HTTP response.
	// Wait for the client acknowledgement before asserting queue and metrics.
	waitFor(t, "delivery after retries", func() bool {
		_, _, succeeded, _ := obs.snapshot()
		return succeeded > 0
	})

	_, _, _, failed := obs.snapshot()
	if failed < 2 {
		t.Fatalf("failed attempts = %d, want at least 2 before success", failed)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("accepted batches = %d, want 1", got)
	}
	if got := c.Queued(); got != 0 {
		t.Fatalf("queued = %d, want 0 after acceptance", got)
	}
}

func TestShutdownFlushesQueuedResults(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	// Interval far longer than the test: only the shutdown flush can deliver.
	c := newClient(t, srv.URL, nil, func(cfg *Config) { cfg.Interval = time.Hour })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	c.Enqueue(testResult("api", time.Now().UTC()))
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if rec.count() != 1 {
		t.Fatalf("batches after shutdown = %d, want 1 (shutdown must not drop results)", rec.count())
	}
}

func TestNextBackoffGrowsAndCaps(t *testing.T) {
	min, max := time.Second, 60*time.Second
	d := time.Duration(0)
	var seen []time.Duration
	for i := 0; i < 10; i++ {
		d = nextBackoff(d, min, max)
		seen = append(seen, d)
		if d < min {
			t.Fatalf("backoff %s below minimum %s", d, min)
		}
		if d > max+max/5+time.Millisecond {
			t.Fatalf("backoff %s exceeded maximum %s plus jitter", d, max)
		}
	}
	if seen[0] > min+min/5+time.Millisecond {
		t.Fatalf("first backoff %s should be about the minimum %s", seen[0], min)
	}
}

func TestNewValidates(t *testing.T) {
	cases := map[string]func(*Config){
		"no url":     func(c *Config) { c.URL = "" },
		"bad scheme": func(c *Config) { c.URL = "ftp://example.com" },
		"no token":   func(c *Config) { c.Token = "" },
	}
	for name, mutate := range cases {
		if _, err := New(Config{URL: "http://example.com", Token: "t"}, nil); err != nil {
			// baseline must be valid
			t.Fatalf("baseline config rejected: %v", err)
		}
		cfg := Config{URL: "http://example.com/api/v1/ingest", Token: "t"}
		mutate(&cfg)
		if _, err := New(cfg, nil); err == nil {
			t.Fatalf("%s: New accepted an invalid config", name)
		}
	}
}

func TestIdlePushSendsHeartbeat(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	c := newClient(t, srv.URL, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "two idle heartbeats", func() bool { return rec.count() >= 2 })
	for _, b := range rec.all() {
		if err := b.Validate(); err != nil || len(b.Results) != 0 || b.InventoryRevision <= 0 {
			t.Fatalf("invalid heartbeat: %+v, %v", b, err)
		}
	}
}
