package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"pulse/internal/model"
)

func ingestAll(t *testing.T, st Store, b model.Batch, now time.Time) {
	t.Helper()
	if n, err := st.Ingest(context.Background(), b, now); n != len(b.Results) || err != nil {
		t.Fatalf("Ingest: n=%d err=%v, want %d new results", n, err, len(b.Results))
	}
}

func snapshotOf(t *testing.T, st Store) Snapshot {
	t.Helper()
	snap, err := st.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestSameTargetOnDifferentProbers(t *testing.T) {
	forEachBackend(t, nil, func(t *testing.T, st Store) {
		ctx := context.Background()
		now := time.Now().UTC()
		a := result("api", now, true, "UP", 40)
		b := result("api", now, false, "DOWN", 0)
		b.Maintenance = true
		ingestAll(t, st, batchOf("b", nil, b), now)
		ingestAll(t, st, batchOf("a", nil, a), now)
		s := snapshotOf(t, st)
		if len(s.Targets) != 2 || len(s.Probers) != 2 {
			t.Fatalf("snapshot = %+v", s)
		}
		for i, want := range []model.Result{a, b} {
			target, p := s.Targets[i], s.Probers[i]
			owner := []string{"a", "b"}[i]
			if target.Prober != owner || p.Info.Name != owner || len(p.Targets) != 1 || p.Targets[0] != target.Spec {
				t.Fatalf("bad inventory: %+v", s)
			}
			if target.State != want.State || target.Last.State != want.State || target.Maintenance != want.Maintenance {
				t.Fatalf("wrong current state for %q: %+v", owner, target)
			}
			for _, w := range UptimeWindows {
				ratio, found := target.Uptime[w.Name]
				if !found || ratio != float64(boolToInt(want.OK)) {
					t.Errorf("%s uptime for %q: %v", w.Name, owner, target.Uptime)
				}
			}
			if (want.OK && target.Latency["p95"] != want.DurationMS) || (!want.OK && len(target.Latency) != 0) {
				t.Errorf("latency for %q: %v", owner, target.Latency)
			}
			h, err := st.History(ctx, owner, "api", now)
			if err != nil || len(h) != 1 || h[0].State != want.State {
				t.Errorf("history for %q: %v, %v", owner, h, err)
			}
		}
		if _, err := st.History(ctx, "", "api", now.Add(time.Hour)); !errors.Is(err, ErrAmbiguousTarget) {
			t.Fatalf("ambiguous history: %v", err)
		}
	})
}

func TestActiveInventoryReconciliation(t *testing.T) {
	forEachBackend(t, nil, func(t *testing.T, st Store) {
		now := time.Now().UTC()
		push := func(specs []model.TargetSpec, name string) {
			now = now.Add(time.Second)
			ingestAll(t, st, batchOf("p", specs, result(name, now, true, "UP", 7)), now)
		}
		check := func(want ...model.TargetSpec) {
			t.Helper()
			s := snapshotOf(t, st)
			if len(s.Targets) != len(want) || len(s.Probers) != 1 || len(s.Probers[0].Targets) != len(want) {
				t.Fatalf("inventory %+v, want %v", s, want)
			}
			for i, spec := range want {
				if s.Targets[i].Spec != spec || s.Probers[0].Targets[i] != spec {
					t.Fatalf("inventory %+v, want %v", s, want)
				}
			}
		}
		old := model.TargetSpec{Name: "old"}
		quiet := model.TargetSpec{Name: "quiet", Group: "new-config"}
		push(nil, "old") // legacy prober: infer inventory
		check(old)
		push([]model.TargetSpec{quiet}, "old") // old sample in the same push as its removal
		check(quiet)
		push(nil, "unseen-backlog") // omitted declaration doesn't replace or extend the list
		check(quiet)
		push([]model.TargetSpec{}, "quiet")
		check()
		push(nil, "old")
		check()
		h, err := st.History(context.Background(), "p", "old", time.Time{})
		if err != nil || len(h) != 3 {
			t.Fatalf("history after removal = %v, %v", h, err)
		}
		push([]model.TargetSpec{old}, "quiet") // only a declaration can reactivate old
		check(old)
	})
}

func TestDelayedTransitionSplitsLatestRun(t *testing.T) {
	forEachBackend(t, nil, func(t *testing.T, st Store) {
		now := time.Now().UTC()
		first, last := now.Add(-2*time.Minute), now
		ingestAll(t, st, batchOf("p", nil, result("api", first, true, "UP", 4), result("api", last, true, "UP", 7)), now)
		delayed := result("api", now.Add(-time.Minute), false, "DOWN", 0)
		delayed.Maintenance = true
		ingestAll(t, st, batchOf("p", nil, delayed), now)
		target := findTarget(t, snapshotOf(t, st), "api")
		if target.State != "UP" || !target.Since.Equal(last) || target.Maintenance || !target.Last.At.Equal(last) {
			t.Fatalf("incorrect state after delayed transition: %+v", target)
		}
		h, err := st.History(context.Background(), "p", "api", first)
		if err != nil || len(h) != 3 || !h[0].At.Equal(first) || !h[1].Maintenance || !h[2].At.Equal(last) {
			t.Fatalf("delayed history: %v, %v", h, err)
		}
	})
}

func TestSQLiteMigratesLegacyStateAndPersistsInventory(t *testing.T) {
	cfg := Config{Kind: "sqlite", Path: filepath.Join(t.TempDir(), "old.db")}
	db, err := sql.Open("sqlite", cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first, down, last := now.Add(-3*time.Minute), now.Add(-2*time.Minute), now.Add(-time.Minute)
	// Old schema with the delayed-state bug: most recent result was UP,
	// but a preceding DOWN arrived last and overwrote targets.state.
	_, err = db.Exec(`CREATE TABLE targets (prober TEXT NOT NULL, name TEXT NOT NULL, grp TEXT NOT NULL DEFAULT '', typ TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'UNKNOWN', since INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(prober,name));
		CREATE TABLE probers (name TEXT PRIMARY KEY, version TEXT NOT NULL DEFAULT '', labels TEXT NOT NULL DEFAULT '{}', last_seen INTEGER NOT NULL, config_hash TEXT NOT NULL DEFAULT '');`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	} // remaining tables
	if _, err = db.Exec(`INSERT INTO probers(name,last_seen) VALUES('p',?)`, now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO targets(prober,name,state,since) VALUES('p','api','DOWN',?),('p','expired','UP',?)`, down.UnixNano(), first.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO results(prober,target,at,ok,state) VALUES('p','api',?,1,'UP'),('p','api',?,0,'DOWN'),('p','api',?,1,'UP')`, first.UnixNano(), down.UnixNano(), last.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ingestAll(t, st, batchOf("p", nil, result("api", last.Add(-time.Second), false, "DOWN", 0), result("expired", first.Add(-time.Second), false, "DOWN", 0)), now)
	snap := snapshotOf(t, st)
	target := findTarget(t, snap, "api")
	if target.State != "UP" || !target.Since.Equal(last) || !target.Last.At.Equal(last) {
		t.Fatalf("legacy state not repaired: %+v", target)
	}
	expired := findTarget(t, snap, "expired")
	if expired.State != "UP" || !expired.Since.Equal(first) || !expired.Last.At.IsZero() {
		t.Fatalf("legacy state without raw data regressed: %+v", expired)
	}
	ingestAll(t, st, batchOf("p", []model.TargetSpec{}, result("api", now, true, "UP", 3)), now)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ingestAll(t, st, batchOf("p", nil, result("new-backlog", now, true, "UP", 2)), now)
	snap = snapshotOf(t, st)
	if len(snap.Targets) != 0 || len(snap.Probers) != 1 || len(snap.Probers[0].Targets) != 0 {
		t.Fatalf("reopen lost empty declaration: %+v", snap)
	}
}
