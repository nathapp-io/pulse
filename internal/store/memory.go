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
	targets map[string]*memTarget
}

type memProber struct {
	info     model.ProberInfo
	lastSeen time.Time
	targets  []model.TargetSpec
}

type memTarget struct {
	spec    model.TargetSpec
	prober  string
	state   string
	since   time.Time
	last    model.Result
	points  []model.Result
	buckets *uptime.Buckets
	seen    map[int64]struct{}
}

func newMemory(cfg Config) *memoryStore {
	return &memoryStore{
		cfg:     cfg,
		probers: make(map[string]*memProber),
		targets: make(map[string]*memTarget),
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
	if len(b.Targets) > 0 {
		p.targets = b.Targets
	}

	// Declared targets exist even before their first result.
	for _, spec := range b.Targets {
		t := m.targetOrCreate(spec.Name, b.Prober.Name)
		t.spec = spec
	}

	stored := 0
	for _, r := range b.Results {
		t := m.targetOrCreate(r.Target, b.Prober.Name)
		if _, dup := t.seen[r.At.UnixNano()]; dup {
			continue
		}
		t.seen[r.At.UnixNano()] = struct{}{}
		t.points = append(t.points, r)
		t.buckets.Record(r.OK, r.At)

		// "Latest" means latest by probe time, not by arrival order.
		if t.last.At.IsZero() || !r.At.Before(t.last.At) {
			t.last = r
			if state := model.NormalizeState(r.State); state != t.state {
				t.state = state
				t.since = r.At
			}
		}
		stored++
	}

	for _, t := range m.targets {
		t.prune(m.cfg, receivedAt)
	}
	return stored, nil
}

func (m *memoryStore) targetOrCreate(name, prober string) *memTarget {
	t := m.targets[name]
	if t == nil {
		t = &memTarget{
			spec:    model.TargetSpec{Name: name},
			prober:  prober,
			state:   "UNKNOWN",
			buckets: uptime.New(),
			seen:    make(map[int64]struct{}),
		}
		m.targets[name] = t
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
			Targets:  p.targets,
		})
	}

	for _, t := range m.targets {
		ts := TargetState{
			Spec:    t.spec,
			Prober:  t.prober,
			State:   t.state,
			Since:   t.since,
			Last:    t.last,
			Uptime:  make(map[string]float64, len(UptimeWindows)),
			Latency: Percentiles(recentDurations(t.points)),
		}
		for _, w := range UptimeWindows {
			if r := t.buckets.Ratio(w.D); r >= 0 {
				ts.Uptime[w.Name] = r
			}
		}
		snap.Targets = append(snap.Targets, ts)
	}

	sortProbers(snap.Probers)
	sortTargets(snap.Targets)
	return snap, nil
}

func (m *memoryStore) History(_ context.Context, target string, from time.Time) ([]model.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t := m.targets[target]
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

// recentDurations returns durations inside the latency window, relative to the
// newest sample (not wall-clock) so a paused instance still reports something.
func recentDurations(points []model.Result) []int64 {
	if len(points) == 0 {
		return nil
	}
	newest := points[len(points)-1].At
	cutoff := newest.Add(-LatencyWindow)

	out := make([]int64, 0, len(points))
	for _, p := range points {
		if !p.At.Before(cutoff) {
			out = append(out, p.DurationMS)
		}
	}
	return out
}

func sortProbers(ps []ProberState) {
	sort.Slice(ps, func(i, j int) bool { return ps[i].Info.Name < ps[j].Info.Name })
}

func sortTargets(ts []TargetState) {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Spec.Name < ts[j].Spec.Name })
}
