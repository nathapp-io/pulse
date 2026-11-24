// Package viewer assembles what the API serves on top of a store: staleness,
// expected-prober detection and the status summary.
//
// The store knows what was reported; only this layer decides what "silent for too
// long" means, so both backends behave the same.
package viewer

import (
	"context"
	"fmt"
	"sort"
	"time"

	"pulse/internal/model"
	"pulse/internal/store"
)

// Config tunes the read-side policy.
type Config struct {
	// StaleAfter is how long a prober may be silent before its targets are
	// reported STALE instead of UP/DOWN.
	StaleAfter time.Duration
	// ExpectedProbers are probers that must report, even if they never have.
	ExpectedProbers []string
}

// Viewer answers status and history questions.
type Viewer struct {
	store store.Store
	cfg   Config
	now   func() time.Time
}

// New builds a viewer. A zero StaleAfter falls back to 30s.
func New(st store.Store, cfg Config) *Viewer {
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 30 * time.Second
	}
	return &Viewer{store: st, cfg: cfg, now: time.Now}
}

// StaleAfter exposes the configured threshold, for startup logging.
func (v *Viewer) StaleAfter() time.Duration { return v.cfg.StaleAfter }

// Ingest stores a batch, stamping the viewer's receive time.
func (v *Viewer) Ingest(ctx context.Context, b model.Batch) (int, error) {
	return v.store.Ingest(ctx, b, v.now().UTC())
}

// History returns a target's results for a rolling window.
func (v *Viewer) History(ctx context.Context, target string, window time.Duration) ([]model.Result, error) {
	if window <= 0 {
		window = time.Hour
	}
	return v.store.History(ctx, target, v.now().UTC().Add(-window))
}

// Status is the payload of GET /api/v1/status.
type Status struct {
	GeneratedAt time.Time `json:"generated_at"`
	StaleAfter  string    `json:"stale_after"`
	Summary     Summary   `json:"summary"`
	Probers     []Prober  `json:"probers"`
	Targets     []Target  `json:"targets"`
}

// Summary is the at-a-glance counters the UI header shows.
type Summary struct {
	Targets        int `json:"targets"`
	Up             int `json:"up"`
	Down           int `json:"down"`
	Stale          int `json:"stale"`
	Unknown        int `json:"unknown"`
	Maintenance    int `json:"maintenance"`
	Probers        int `json:"probers"`
	ProbersStale   int `json:"probers_stale"`
	ProbersMissing int `json:"probers_missing"`
}

// Prober is one prober's health as the viewer sees it.
type Prober struct {
	Name     string            `json:"name"`
	Version  string            `json:"version"`
	Labels   map[string]string `json:"labels,omitempty"`
	State    string            `json:"state"` // HEALTHY | STALE | MISSING
	LastSeen *time.Time        `json:"last_seen,omitempty"`
	LagMS    int64             `json:"lag_ms"`
	Targets  int               `json:"targets"`
}

// Target is one target's health as the viewer sees it.
type Target struct {
	Name   string    `json:"name"`
	Group  string    `json:"group"`
	Type   string    `json:"type"`
	Prober string    `json:"prober"`
	State  string    `json:"state"` // UP | DOWN | UNKNOWN | STALE | MAINTENANCE
	Since  time.Time `json:"since"`
	Stale  bool      `json:"stale"`
	// Maintenance is set when the target's latest result was taken inside a
	// declared maintenance window: still recorded, no longer counted as DOWN.
	Maintenance bool               `json:"maintenance,omitempty"`
	Last        *model.Result      `json:"last,omitempty"`
	Uptime      map[string]float64 `json:"uptime,omitempty"`
	LatencyMS   map[string]int64   `json:"latency_ms,omitempty"`
}

// Status builds the current view.
func (v *Viewer) Status(ctx context.Context) (Status, error) {
	snap, err := v.store.Snapshot(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("snapshot: %w", err)
	}
	now := v.now().UTC()

	st := Status{
		GeneratedAt: now,
		StaleAfter:  v.cfg.StaleAfter.String(),
		Probers:     make([]Prober, 0, len(snap.Probers)),
		Targets:     make([]Target, 0, len(snap.Targets)),
		Summary:     Summary{Targets: len(snap.Targets)},
	}

	// Prober health, including expected probers that never reported.
	proberStale := make(map[string]bool, len(snap.Probers))
	seen := make(map[string]bool, len(snap.Probers))
	for _, p := range snap.Probers {
		lag := now.Sub(p.LastSeen)
		stale := lag > v.cfg.StaleAfter
		proberStale[p.Info.Name] = stale
		seen[p.Info.Name] = true

		lastSeen := p.LastSeen
		entry := Prober{
			Name:     p.Info.Name,
			Version:  p.Info.Version,
			Labels:   p.Info.Labels,
			State:    "HEALTHY",
			LastSeen: &lastSeen,
			LagMS:    lag.Milliseconds(),
			Targets:  len(p.Targets),
		}
		if stale {
			entry.State = "STALE"
			st.Summary.ProbersStale++
		}
		st.Probers = append(st.Probers, entry)
	}
	for _, name := range v.cfg.ExpectedProbers {
		if seen[name] {
			continue
		}
		proberStale[name] = true
		st.Probers = append(st.Probers, Prober{Name: name, State: "MISSING"})
		st.Summary.ProbersMissing++
	}
	st.Summary.Probers = len(st.Probers)

	// Target health. A target whose prober is silent is STALE, never DOWN:
	// the last observation is still the last thing we know to be true.
	for _, t := range snap.Targets {
		stale := proberStale[t.Prober]
		state := t.State
		if state == "" {
			state = "UNKNOWN"
		}
		if stale {
			state = "STALE"
		} else if t.Maintenance {
			// A planned outage is not a failure. The raw result is still stored
			// and visible in history, so nothing is hidden -- only not alarmed on.
			state = "MAINTENANCE"
		}

		entry := Target{
			Name:        t.Spec.Name,
			Group:       t.Spec.Group,
			Type:        t.Spec.Type,
			Prober:      t.Prober,
			State:       state,
			Since:       t.Since,
			Stale:       stale,
			Maintenance: t.Maintenance,
			Uptime:      t.Uptime,
			LatencyMS:   t.Latency,
		}
		if !t.Last.At.IsZero() {
			last := t.Last
			entry.Last = &last
		}
		st.Targets = append(st.Targets, entry)

		switch state {
		case "UP":
			st.Summary.Up++
		case "DOWN":
			st.Summary.Down++
		case "STALE":
			st.Summary.Stale++
		case "MAINTENANCE":
			st.Summary.Maintenance++
		default:
			st.Summary.Unknown++
		}
	}

	sort.Slice(st.Probers, func(i, j int) bool { return st.Probers[i].Name < st.Probers[j].Name })
	sort.Slice(st.Targets, func(i, j int) bool { return st.Targets[i].Name < st.Targets[j].Name })
	return st, nil
}
