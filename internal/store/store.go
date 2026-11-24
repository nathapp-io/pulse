// Package store persists probe results for the viewer.
//
// Two backends implement the same interface:
//
//	memory - bounded rings per target, zero setup, history lost on restart
//	sqlite - durable, single writer, rollups for long windows
//
// Both are written for the under-100-target scale the project targets: reads are
// simple queries, writes are batched per push, and no backend needs a server.
package store

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"pulse/internal/model"
)

// UptimeWindows are the rolling windows reported to the UI and the API.
var UptimeWindows = []struct {
	Name string
	D    time.Duration
}{
	{"1h", time.Hour},
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
}

// LatencyWindow is the span used for p50/p95, which comes from raw results.
const LatencyWindow = time.Hour

// Config selects and tunes a backend.
type Config struct {
	// Kind is "memory" or "sqlite".
	Kind string
	// Path is the SQLite file (sqlite only).
	Path string
	// Retention is how long raw results are kept.
	Retention time.Duration
	// Retention1M and Retention1H bound the rollup tables (sqlite only).
	Retention1M time.Duration
	Retention1H time.Duration
	// MaxPerTarget is the raw ring size for the memory backend.
	MaxPerTarget int
}

func (c *Config) applyDefaults() {
	if c.Kind == "" {
		c.Kind = "memory"
	}
	if c.Retention <= 0 {
		c.Retention = 7 * 24 * time.Hour
	}
	if c.Retention1M <= 0 {
		c.Retention1M = 30 * 24 * time.Hour
	}
	if c.Retention1H <= 0 {
		c.Retention1H = 365 * 24 * time.Hour
	}
	if c.MaxPerTarget <= 0 {
		c.MaxPerTarget = 2880 // 24h at 30s intervals
	}
}

// Open builds the configured backend.
func Open(cfg Config) (Store, error) {
	cfg.applyDefaults()
	switch cfg.Kind {
	case "memory":
		return newMemory(cfg), nil
	case "sqlite":
		if cfg.Path == "" {
			return nil, fmt.Errorf("store sqlite requires a path (PULSE_DB)")
		}
		return openSQLite(cfg)
	default:
		return nil, fmt.Errorf("unknown store kind %q (want memory or sqlite)", cfg.Kind)
	}
}

// Store persists results and answers the viewer's read patterns.
type Store interface {
	// Ingest stores a batch idempotently, keyed by (prober, target, at), and
	// returns how many results were new.
	Ingest(ctx context.Context, b model.Batch, receivedAt time.Time) (int, error)
	// Snapshot returns the current state of every prober and target.
	Snapshot(ctx context.Context) (Snapshot, error)
	// History returns a target's stored results at or after from, oldest first.
	History(ctx context.Context, target string, from time.Time) ([]model.Result, error)
	// Close releases backend resources.
	Close() error
}

// Snapshot is the current view of the whole installation.
type Snapshot struct {
	Probers []ProberState
	Targets []TargetState
}

// ProberState is one prober's last contact.
type ProberState struct {
	Info     model.ProberInfo
	LastSeen time.Time
	Targets  []model.TargetSpec
}

// TargetState is one target's latest health, history aggregates included.
type TargetState struct {
	Spec   model.TargetSpec
	Prober string
	State  string
	Since  time.Time
	Last   model.Result
	// Maintenance is true when the latest sample was taken inside a maintenance
	// window, so a viewer can show the target differently without touching the
	// recorded result.
	Maintenance bool
	Uptime      map[string]float64
	Latency     map[string]int64
}

// Percentiles computes p50/p95 from raw durations, in milliseconds.
func Percentiles(durations []int64) map[string]int64 {
	out := make(map[string]int64, 2)
	if len(durations) == 0 {
		return out
	}
	sorted := make([]int64, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	// Nearest-rank: p95 of five samples is the largest one, which is what an
	// operator expects from "p95 latency" on a short window.
	at := func(p float64) int64 {
		idx := int(math.Ceil(p*float64(len(sorted)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}
	out["p50"] = at(0.50)
	out["p95"] = at(0.95)
	return out
}

// Ratio returns ok/total, or -1 when there is no data for the window.
func Ratio(ok, total int64) float64 {
	if total <= 0 {
		return -1
	}
	return float64(ok) / float64(total)
}
