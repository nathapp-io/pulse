package store

import (
	"context"
	"sort"
	"sync"
	"time"

	"pulse/internal/model"
	"pulse/internal/uptime"
)

// memoryStore keeps everything in bounded rings. History is lost on restart,
// which is the documented trade-off of this backend.
type memoryStore struct {
	cfg Config

	mu      sync.Mutex
	probers map[string]*memProber
	targets map[targetKey]*memTarget
}

type memProber struct {
	order    inventoryOrder
	info     model.ProberInfo
	lastSeen time.Time
	declared bool // once a list is supplied, results alone cannot activate targets
}

type memTarget struct {
	active bool
	spec   model.TargetSpec
	prober string
	stateRun
	last    model.Result
	points  []model.Result
	buckets *uptime.Buckets
	seen    map[int64]struct{}
}

func newMemory(cfg Config) *memoryStore {
	return &memoryStore{
		cfg:     cfg,
		probers: make(map[string]*memProber),
		targets: make(map[targetKey]*memTarget),
	}
}

func (m *memoryStore) Ingest(_ context.Context, b model.Batch, receivedAt time.Time) (int, error) {
	if err := b.Validate(); err != nil {
		return 0, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	p := m.probers[b.Prober.Name]
	if p == nil {
		p = &memProber{}
		m.probers[b.Prober.Name] = p
	}
	p.info = b.Prober
	p.lastSeen = receivedAt
	if b.Targets != nil {
		order := declarationOrder(b)
		if !p.declared || order.newerThan(p.order) {
			p.declared, p.order = true, order
			for k, t := range m.targets {
				if k.prober == b.Prober.Name {
					t.active = false
				}
			}
			// Declared targets exist even before their first result.
			for _, spec := range b.Targets {
				t := m.targetOrCreate(spec.Name, b.Prober.Name)
				t.spec, t.active = spec, true
			}
		}
	}

	stored := 0
	for _, r := range b.Results {
		t := m.targetOrCreate(r.Target, b.Prober.Name)
		if !p.declared {
			t.active = true
		}
		if _, dup := t.seen[r.At.UnixNano()]; dup {
			continue
		}
		t.seen[r.At.UnixNano()] = struct{}{}
		// Keep history in probe-time order even when a backlog arrives late.
		i := len(t.points)
		if i > 0 && r.At.Before(t.points[i-1].At) {
			i = sort.Search(i, func(i int) bool { return !t.points[i].At.Before(r.At) })
		}
		t.points = append(t.points, r)
		copy(t.points[i+1:], t.points[i:len(t.points)-1])
		t.points[i] = r
		t.buckets.Record(r.OK, r.At)

		if t.latest.IsZero() || r.At.After(t.latest) {
			t.last = r
		}
		var next time.Time
		if i+1 < len(t.points) {
			next = t.points[i+1].At
		}
		t.record(model.NormalizeState(r.State), r.At, next)
		stored++
	}

	for _, t := range m.targets {
		t.prune(m.cfg, receivedAt)
	}
	return stored, nil
}

func (m *memoryStore) targetOrCreate(name, prober string) *memTarget {
	key := targetKey{prober, name}
	t := m.targets[key]
	if t == nil {
		t = &memTarget{
			spec:     model.TargetSpec{Name: name},
			prober:   prober,
			stateRun: stateRun{state: "UNKNOWN"},
			buckets:  uptime.New(),
			seen:     make(map[int64]struct{}),
		}
		m.targets[key] = t
	}
	return t
}

// prune drops results outside the retention window, then enforces the ring size.
func (t *memTarget) prune(cfg Config, now time.Time) {
	cutoff := now.Add(-cfg.Retention).UnixNano()

	kept := t.points[:0]
	for _, p := range t.points {
		if p.At.UnixNano() < cutoff {
			delete(t.seen, p.At.UnixNano())
			continue
		}
		kept = append(kept, p)
	}
	t.points = kept

	if extra := len(t.points) - cfg.MaxPerTarget; extra > 0 {
		for _, p := range t.points[:extra] {
			delete(t.seen, p.At.UnixNano())
		}
		t.points = t.points[extra:]
	}
}

func (m *memoryStore) Snapshot(_ context.Context) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	snap := Snapshot{
		Probers: make([]ProberState, 0, len(m.probers)),
		Targets: make([]TargetState, 0, len(m.targets)),
	}

	for _, p := range m.probers {
		snap.Probers = append(snap.Probers, ProberState{
			Info:     p.info,
			LastSeen: p.lastSeen,
		})
	}

	for _, t := range m.targets {
		if !t.active {
			continue
		}
		ts := TargetState{
			Spec:        t.spec,
			Prober:      t.prober,
			State:       t.state,
			Since:       t.since,
			Last:        t.last,
			Maintenance: t.last.Maintenance,
			Uptime:      make(map[string]float64, len(UptimeWindows)),
			Latency:     Percentiles(recentDurations(t.points)),
		}
		for _, w := range UptimeWindows {
			if r := t.buckets.Ratio(w.D); r >= 0 {
				ts.Uptime[w.Name] = r
			}
		}
		snap.Targets = append(snap.Targets, ts)
	}

	snap.finish()
	return snap, nil
}

func (m *memoryStore) History(_ context.Context, prober, target string, from time.Time) ([]model.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if prober == "" {
		for k := range m.targets {
			if k.name != target {
				continue
			}
			if prober != "" {
				return nil, ErrAmbiguousTarget
			}
			prober = k.prober
		}
	}
	t := m.targets[targetKey{prober, target}]
	if t == nil {
		return nil, nil
	}
	out := make([]model.Result, 0, len(t.points))
	for _, p := range t.points {
		if !p.At.Before(from) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (m *memoryStore) Close() error { return nil }

// recentDurations returns durations of successful probes inside the latency
// window, relative to the newest sample (not wall-clock) so a paused instance
// still reports something.
//
// Failures are excluded: a refused connection records 0 ms, so counting it would
// report a dead target as fast and drag a flaky target's percentiles towards zero.
func recentDurations(points []model.Result) []int64 {
	if len(points) == 0 {
		return nil
	}
	newest := points[len(points)-1].At
	cutoff := newest.Add(-LatencyWindow)

	out := make([]int64, 0, len(points))
	for _, p := range points {
		if p.OK && !p.At.Before(cutoff) {
			out = append(out, p.DurationMS)
		}
	}
	return out
}
