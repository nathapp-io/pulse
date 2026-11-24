package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"pulse/internal/model"
)

// forEachBackend runs the same assertions against every backend, so the two
// implementations cannot drift apart in behaviour.
func forEachBackend(t *testing.T, mutate func(*Config), fn func(t *testing.T, st Store)) {
	t.Helper()
	for _, kind := range []string{"memory", "sqlite"} {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			cfg := Config{
				Kind:         kind,
				Path:         filepath.Join(t.TempDir(), "pulse.db"),
				Retention:    7 * 24 * time.Hour,
				MaxPerTarget: 500,
			}
			if mutate != nil {
				mutate(&cfg)
			}
			st, err := Open(cfg)
			if err != nil {
				t.Fatalf("Open(%s): %v", kind, err)
			}
			t.Cleanup(func() { _ = st.Close() })
			fn(t, st)
		})
	}
}

func result(target string, at time.Time, ok bool, state string, durationMS int64) model.Result {
	r := model.Result{
		Target:     target,
		At:         at,
		OK:         ok,
		State:      state,
		DurationMS: durationMS,
	}
	if !ok {
		r.ErrorClass = "CONNECT_REFUSED"
		r.Error = "connection refused"
	}
	return r
}

func batchOf(prober string, specs []model.TargetSpec, results ...model.Result) model.Batch {
	return model.Batch{
		Prober:  model.ProberInfo{Name: prober, Version: "test"},
		Targets: specs,
		Results: results,
	}
}

func findTarget(t *testing.T, snap Snapshot, name string) TargetState {
	t.Helper()
	for _, ts := range snap.Targets {
		if ts.Spec.Name == name {
			return ts
		}
	}
	t.Fatalf("target %q not in snapshot (%d targets)", name, len(snap.Targets))
	return TargetState{}
}

func TestIngestAndSnapshot(t *testing.T) {
	forEachBackend(t, nil, func(t *testing.T, st Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		specs := []model.TargetSpec{{Name: "api", Group: "web", Type: "http"}}

		n, err := st.Ingest(ctx, batchOf("prober-a", specs,
			result("api", now, true, "UP", 12),
		), now)
		if err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		if n != 1 {
			t.Fatalf("stored = %d, want 1", n)
		}

		snap, err := st.Snapshot(ctx)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if len(snap.Probers) != 1 || snap.Probers[0].Info.Name != "prober-a" {
			t.Fatalf("probers = %+v, want one prober-a", snap.Probers)
		}
		if !snap.Probers[0].LastSeen.Equal(now) {
			t.Fatalf("lastSeen = %s, want %s", snap.Probers[0].LastSeen, now)
		}

		target := findTarget(t, snap, "api")
		if target.State != "UP" {
			t.Fatalf("state = %q, want UP", target.State)
		}
		if target.Prober != "prober-a" {
			t.Fatalf("prober = %q, want prober-a", target.Prober)
		}
		if target.Spec.Group != "web" || target.Spec.Type != "http" {
			t.Fatalf("spec = %+v, want group web type http", target.Spec)
		}
		if !target.Last.At.Equal(now) || !target.Last.OK {
			t.Fatalf("last = %+v, want the stored result", target.Last)
		}
	})
}

func TestIngestIsIdempotent(t *testing.T) {
	forEachBackend(t, nil, func(t *testing.T, st Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		b := batchOf("prober-a", nil,
			result("api", now, true, "UP", 10),
			result("api", now.Add(time.Second), true, "UP", 11),
		)

		if n, err := st.Ingest(ctx, b, now); err != nil || n != 2 {
			t.Fatalf("first ingest: n=%d err=%v, want 2 nil", n, err)
		}
		// A retried push must store nothing new.
		if n, err := st.Ingest(ctx, b, now); err != nil || n != 0 {
			t.Fatalf("replay: n=%d err=%v, want 0 nil", n, err)
		}

		history, err := st.History(ctx, "api", now.Add(-time.Hour))
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(history) != 2 {
			t.Fatalf("history length = %d after replay, want 2", len(history))
		}
	})
}

func TestStateTransitionRecordsSince(t *testing.T) {
	forEachBackend(t, nil, func(t *testing.T, st Store) {
		ctx := context.Background()
		base := time.Now().UTC().Truncate(time.Second)

		if _, err := st.Ingest(ctx, batchOf("prober-a", nil,
			result("api", base, true, "UP", 5),
		), base); err != nil {
			t.Fatalf("Ingest up: %v", err)
		}
		// Two DOWN results: Since must be the first DOWN, not the latest sample.
		downAt := base.Add(30 * time.Second)
		if _, err := st.Ingest(ctx, batchOf("prober-a", nil,
			result("api", downAt, false, "DOWN", 0),
			result("api", downAt.Add(30*time.Second), false, "DOWN", 0),
		), downAt.Add(30*time.Second)); err != nil {
			t.Fatalf("Ingest down: %v", err)
		}

		snap, err := st.Snapshot(ctx)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		target := findTarget(t, snap, "api")
		if target.State != "DOWN" {
			t.Fatalf("state = %q, want DOWN", target.State)
		}
		if !target.Since.Equal(downAt) {
			t.Fatalf("since = %s, want %s (the transition, not the latest sample)", target.Since, downAt)
		}
	})
}

func TestUptimeRatio(t *testing.T) {
	forEachBackend(t, nil, func(t *testing.T, st Store) {
		ctx := context.Background()
		base := time.Now().UTC().Truncate(time.Second)

		if _, err := st.Ingest(ctx, batchOf("prober-a", nil,
			result("api", base, true, "UP", 5),
			result("api", base.Add(10*time.Second), false, "DOWN", 0),
			result("api", base.Add(20*time.Second), true, "UP", 6),
		), base.Add(20*time.Second)); err != nil {
			t.Fatalf("Ingest: %v", err)
		}

		snap, err := st.Snapshot(ctx)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		target := findTarget(t, snap, "api")
		got, ok := target.Uptime["1h"]
		if !ok {
			t.Fatalf("no 1h uptime in %+v", target.Uptime)
		}
		if want := 2.0 / 3.0; got < want-0.001 || got > want+0.001 {
			t.Fatalf("uptime 1h = %v, want %v", got, want)
		}
	})
}

func TestLatencyPercentiles(t *testing.T) {
	forEachBackend(t, nil, func(t *testing.T, st Store) {
		ctx := context.Background()
		base := time.Now().UTC().Truncate(time.Second)

		var results []model.Result
		for i, d := range []int64{10, 20, 30, 40, 100} {
			results = append(results, result("api", base.Add(time.Duration(i)*time.Second), true, "UP", d))
		}
		if _, err := st.Ingest(ctx, batchOf("prober-a", nil, results...), base.Add(10*time.Second)); err != nil {
			t.Fatalf("Ingest: %v", err)
		}

		snap, err := st.Snapshot(ctx)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		target := findTarget(t, snap, "api")
		if target.Latency["p50"] != 30 {
			t.Fatalf("p50 = %d, want 30 (%+v)", target.Latency["p50"], target.Latency)
		}
		if target.Latency["p95"] != 100 {
			t.Fatalf("p95 = %d, want 100 (%+v)", target.Latency["p95"], target.Latency)
		}
	})
}

func TestHistoryWindow(t *testing.T) {
	forEachBackend(t, nil, func(t *testing.T, st Store) {
		ctx := context.Background()
		base := time.Now().UTC().Truncate(time.Second)

		if _, err := st.Ingest(ctx, batchOf("prober-a", nil,
			result("api", base.Add(-2*time.Minute), true, "UP", 1),
			result("api", base, true, "UP", 2),
			result("other", base, true, "UP", 3),
		), base); err != nil {
			t.Fatalf("Ingest: %v", err)
		}

		history, err := st.History(ctx, "api", base.Add(-time.Minute))
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(history) != 1 {
			t.Fatalf("history length = %d, want 1 (older sample filtered by from)", len(history))
		}
		if !history[0].At.Equal(base) {
			t.Fatalf("history[0].At = %s, want %s", history[0].At, base)
		}
	})
}

func TestRetentionPrunesOldResults(t *testing.T) {
	forEachBackend(t, func(cfg *Config) { cfg.Retention = time.Hour }, func(t *testing.T, st Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		// One sample outside the window, one inside.
		if _, err := st.Ingest(ctx, batchOf("prober-a", nil,
			result("api", now.Add(-3*time.Hour), true, "UP", 1),
			result("api", now, true, "UP", 2),
		), now); err != nil {
			t.Fatalf("Ingest: %v", err)
		}

		history, err := st.History(ctx, "api", time.Time{})
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		if len(history) != 1 {
			t.Fatalf("history length = %d, want 1 (the old sample must be pruned)", len(history))
		}
		if !history[0].At.Equal(now) {
			t.Fatalf("kept sample at %s, want %s", history[0].At, now)
		}
	})
}

func TestRejectsInvalidBatch(t *testing.T) {
	forEachBackend(t, nil, func(t *testing.T, st Store) {
		ctx := context.Background()
		now := time.Now().UTC()

		cases := map[string]model.Batch{
			"no prober name": {Results: []model.Result{result("api", now, true, "UP", 1)}},
			"no results":     batchOf("prober-a", nil),
			"no target":      batchOf("prober-a", nil, model.Result{At: now}),
			"no timestamp":   batchOf("prober-a", nil, model.Result{Target: "api"}),
			"duplicate": batchOf("prober-a", nil,
				result("api", now, true, "UP", 1),
				result("api", now, true, "UP", 2),
			),
		}
		for name, b := range cases {
			if _, err := st.Ingest(ctx, b, now); err == nil {
				t.Fatalf("%s: Ingest accepted an invalid batch", name)
			}
		}
	})
}

func TestOpenRejectsUnknownKind(t *testing.T) {
	if _, err := Open(Config{Kind: "postgres"}); err == nil {
		t.Fatal("Open accepted an unknown store kind")
	} else {
		t.Logf("rejected as expected: %v", err)
	}
}

func TestOpenSQLiteNeedsPath(t *testing.T) {
	if _, err := Open(Config{Kind: "sqlite"}); err == nil {
		t.Fatal("Open accepted sqlite without a path")
	}
}

// TestSnapshotIncludesQuietTargets guards the case where a prober declares a
// target but that target has no result yet: it must still be visible as UNKNOWN.
func TestSnapshotIncludesQuietTargets(t *testing.T) {
	forEachBackend(t, nil, func(t *testing.T, st Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		specs := []model.TargetSpec{
			{Name: "declared", Group: "web", Type: "http"},
			{Name: "measured", Group: "web", Type: "http"},
		}
		if _, err := st.Ingest(ctx, batchOf("prober-a", specs,
			result("measured", now, true, "UP", 1),
		), now); err != nil {
			t.Fatalf("Ingest: %v", err)
		}

		snap, err := st.Snapshot(ctx)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		quiet := findTarget(t, snap, "declared")
		if quiet.State != "UNKNOWN" {
			t.Fatalf("quiet target state = %q, want UNKNOWN", quiet.State)
		}
		if quiet.Spec.Group != "web" {
			t.Fatalf("quiet target spec = %+v, want the declared group", quiet.Spec)
		}
	})
}
