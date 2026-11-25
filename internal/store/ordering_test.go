package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"pulse/internal/model"
)

func reopenIfSQLite(t *testing.T, st Store) Store {
	t.Helper()
	s, ok := st.(*sqliteStore)
	if !ok {
		return st
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

func TestInventoryOrdering(t *testing.T) {
	for _, protocol := range []string{"legacy", "versioned"} {
		for _, arrival := range []string{"old-first", "new-first"} {
			t.Run(protocol+"/"+arrival, func(t *testing.T) {
				forEachBackend(t, nil, func(t *testing.T, st Store) {
					now := time.Now().UTC()
					sample := func(name string, sec int) model.Result {
						return result(name, now.Add(time.Duration(sec)*time.Second), true, "UP", 1)
					}
					old := batchOf("p", []model.TargetSpec{{Name: "old"}}, sample("old", 2))
					want := model.TargetSpec{Name: "current", Group: "new"}
					// The greatest measurement time isn't necessarily the last result.
					current := batchOf("p", []model.TargetSpec{want}, sample("api", 3), sample("api", 1))
					if protocol == "versioned" {
						old.InventoryRevision, current.InventoryRevision = 1, 2
						old.Results[0] = sample("old", 10) // older declaration, newer measurement
					}
					if arrival == "old-first" {
						ingestAll(t, st, old, now)
					}
					ingestAll(t, st, current, now)
					st = reopenIfSQLite(t, st)
					if arrival == "new-first" {
						ingestAll(t, st, old, now)
					}
					// Repeat both an equal marker (with new data/metadata) and an exact old batch.
					equal := current
					equal.Targets = []model.TargetSpec{{Name: "current", Group: "stale"}}
					equal.Results = []model.Result{sample("other", 3)}
					ingestAll(t, st, equal, now)
					if n, err := st.Ingest(context.Background(), old, now); err != nil || n != 0 {
						t.Fatalf("old replay: %d, %v", n, err)
					}
					s := snapshotOf(t, st)
					if len(s.Targets) != 1 || s.Targets[0].Spec != want || len(s.Probers[0].Targets) != 1 || s.Probers[0].Targets[0] != want {
						t.Fatalf("stale declaration replaced inventory: %+v", s)
					}
					for _, name := range []string{"old", "other"} {
						h, err := st.History(context.Background(), "p", name, now)
						if err != nil || len(h) != 1 {
							t.Fatalf("history of %s: %v, %v", name, h, err)
						}
					}
				})
			})
		}
	}
}

func TestInventoryProtocolUpgrade(t *testing.T) {
	forEachBackend(t, nil, func(t *testing.T, st Store) {
		now := time.Now().UTC()
		send := func(prober string, rev int64, sec int, targets []model.TargetSpec) {
			b := batchOf(prober, targets, result("sample", now.Add(time.Duration(sec)*time.Second), true, "UP", 1))
			b.InventoryRevision = rev
			ingestAll(t, st, b, now)
		}
		specs := []model.TargetSpec{{Name: "legacy"}}
		send("p", 0, 10, specs)
		send("p", 0, 9, nil)
		send("p", 1, 1, []model.TargetSpec{}) // older samples; newer protocol
		st = reopenIfSQLite(t, st)
		send("p", 0, 11, specs) // no downgrade
		send("q", 0, 1, specs)  // another prober still uses legacy ordering
		s := snapshotOf(t, st)
		if len(s.Targets) != 1 || s.Targets[0].Prober != "q" || s.Targets[0].Spec != specs[0] {
			t.Fatalf("unexpected inventory: %+v", s)
		}
	})
}

func TestSQLiteInventoryRollback(t *testing.T) {
	st, err := Open(Config{Kind: "sqlite", Path: filepath.Join(t.TempDir(), "tx.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	b := batchOf("p", []model.TargetSpec{{Name: "old"}}, result("old", now, true, "UP", 1))
	b.InventoryRevision = 1
	ingestAll(t, st, b, now)
	db := st.(*sqliteStore).db
	if _, err := db.Exec(`CREATE TRIGGER fail_insert BEFORE INSERT ON results BEGIN SELECT RAISE(ABORT,'injected failure'); END;`); err != nil {
		t.Fatal(err)
	}
	b.InventoryRevision = 2
	b.Targets = []model.TargetSpec{}
	if _, err := st.Ingest(context.Background(), b, now); err == nil {
		t.Fatal("expected result insert failure")
	}
	if got := snapshotOf(t, st).Targets; len(got) != 1 || got[0].Spec.Name != "old" {
		t.Fatalf("failed batch changed membership: %v", got)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_insert`); err != nil {
		t.Fatal(err)
	}
	// The failed batch didn't consume revision 2. Even a batch of entirely
	// duplicate results can carry a genuinely newer declaration.
	if n, err := st.Ingest(context.Background(), b, now); err != nil || n != 0 {
		t.Fatalf("retry: %d, %v", n, err)
	}
	if got := snapshotOf(t, st).Targets; len(got) != 0 {
		t.Fatalf("retry didn't remove old target: %v", got)
	}
}

func TestLateSamplesExtendAndSplitRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []int
	}{
		{"ordered", []int{0, 1, 2, 3, 4}},
		{"late-transition", []int{0, 4, 3, 1, 2}},
		{"late-predecessors", []int{4, 1, 3, 2, 0}},
		{"reverse", []int{4, 3, 2, 1, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forEachBackend(t, nil, func(t *testing.T, st Store) {
				base := time.Now().UTC()
				states := []string{"UP", "DOWN", "UP", "UP", "UP"}
				var accepted [5]bool
				for _, i := range tc.events {
					at := func(j int) time.Time { return base.Add(time.Duration(j) * time.Minute) }
					b := batchOf("p", nil, result("api", at(i), states[i] == "UP", states[i], 1))
					ingestAll(t, st, b, base)
					accepted[i] = true
					if n, err := st.Ingest(context.Background(), b, base); n != 0 || err != nil {
						t.Fatalf("replay: %d %v", n, err)
					}
					latest, first := -1, -1
					for j, ok := range accepted {
						if ok {
							if latest < 0 || states[j] != states[latest] {
								first = j
							}
							latest = j
						}
					}
					target := findTarget(t, snapshotOf(t, st), "api")
					if target.State != states[latest] || !target.Since.Equal(at(first)) || !target.Last.At.Equal(at(latest)) {
						t.Fatalf("after event %d: since=%s latest=%s state=%s; want %s %s %s", i, target.Since, target.Last.At, target.State, at(first), at(latest), states[latest])
					}
				}
			})
		})
	}
}

func TestRunBoundarySurvivesRetention(t *testing.T) {
	forEachBackend(t, func(cfg *Config) {
		if cfg.Kind == "memory" {
			cfg.MaxPerTarget = 1
		} else {
			cfg.Retention = time.Minute
		}
	}, func(t *testing.T, st Store) {
		now := time.Now().UTC()
		at := func(min int) time.Time { return now.Add(time.Duration(min) * time.Minute) }
		send := func(min int, state string) {
			ingestAll(t, st, batchOf("p", nil, result("api", at(min), state == "UP", state, 1)), now)
		}
		check := func(min int) {
			t.Helper()
			target := findTarget(t, snapshotOf(t, st), "api")
			if !target.Since.Equal(at(min)) {
				t.Fatalf("since=%s want %s", target.Since, at(min))
			}
		}
		ingestAll(t, st, batchOf("p", nil, result("api", at(-8), false, "DOWN", 0), result("api", at(-5), true, "UP", 1), result("api", now, true, "UP", 1)), now)
		check(-5)
		if h, err := st.History(context.Background(), "p", "api", at(-10)); err != nil || len(h) != 1 {
			t.Fatalf("expected only latest raw sample: %v %v", h, err)
		}
		st = reopenIfSQLite(t, st)
		send(-9, "UP")
		check(-5) // outside the run; don't cross the evicted/pruned DOWN
		send(-7, "UP")
		check(-7) // inside the gap; don't clamp to retention
		send(-4, "DOWN")
		check(0) // split in expired history; earliest surviving successor
		send(-3, "UP")
		check(-3)
	})
}

func TestSQLiteMigratesBranchWatermarks(t *testing.T) {
	cfg := Config{Kind: "sqlite", Path: filepath.Join(t.TempDir(), "branch.db")}
	db, err := sql.Open("sqlite", cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	at := func(i int) time.Time { return now.Add(time.Duration(i) * time.Minute) }
	// Branch tables already have declared/latest_at, but not either ordering fix.
	_, err = db.Exec(`CREATE TABLE probers(name TEXT PRIMARY KEY, version TEXT NOT NULL DEFAULT '', labels TEXT NOT NULL DEFAULT '{}', last_seen INTEGER NOT NULL, config_hash TEXT NOT NULL DEFAULT '', declared INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE targets(prober TEXT NOT NULL, name TEXT NOT NULL, grp TEXT NOT NULL DEFAULT '', typ TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'UNKNOWN', since INTEGER NOT NULL DEFAULT 0, active INTEGER NOT NULL DEFAULT 1, latest_at INTEGER, PRIMARY KEY(prober,name));` + schema)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO probers(name,last_seen,declared) VALUES('p',?,1)`, now.UnixNano())
	exec(`INSERT INTO targets(prober,name,state,since,latest_at,active) VALUES('p','api','UP',?,?,1),('p','expired','UP',?,?,0)`, at(0).UnixNano(), at(4).UnixNano(), at(5).UnixNano(), at(5).UnixNano())
	exec(`INSERT INTO results(prober,target,at,state,ok) VALUES('p','api',?,'UP',1),('p','api',?,'DOWN',0),('p','api',?,'UP',1),('p','api',?,'UP',1)`, at(0).UnixNano(), at(1).UnixNano(), at(3).UnixNano(), at(4).UnixNano())
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	check := func(since int) {
		t.Helper()
		s := snapshotOf(t, st)
		if len(s.Targets) != 1 || s.Targets[0].Spec.Name != "api" || !s.Targets[0].Since.Equal(at(since)) {
			t.Fatalf("migrated snapshot: %+v, want only api since %s", s, at(since))
		}
	}
	check(3)
	st = reopenIfSQLite(t, st)
	// A legacy declaration at the previous maximum is stale, even if the
	// target holding that watermark was inactive and its raw samples pruned.
	ingestAll(t, st, batchOf("p", []model.TargetSpec{}, result("other", at(5), true, "UP", 1)), now)
	ingestAll(t, st, batchOf("p", nil, result("api", at(-1), true, "UP", 1)), now)
	check(3)
	ingestAll(t, st, batchOf("p", nil, result("api", at(2), true, "UP", 1)), now)
	check(2)
}
