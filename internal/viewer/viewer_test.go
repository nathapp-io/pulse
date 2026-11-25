package viewer

import (
	"context"
	"testing"
	"time"

	"pulse/internal/model"
	"pulse/internal/store"
)

func newTestViewer(t *testing.T, cfg Config) (*Viewer, *time.Time) {
	t.Helper()
	st, err := store.Open(store.Config{Kind: "memory", Retention: time.Hour, MaxPerTarget: 100})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().UTC()
	v := New(st, cfg)
	v.now = func() time.Time { return now }
	return v, &now
}

func ingestUp(t *testing.T, v *Viewer, at time.Time) {
	t.Helper()
	b := model.Batch{
		Prober:  model.ProberInfo{Name: "prober-a", Version: "test"},
		Targets: []model.TargetSpec{{Name: "api", Group: "web", Type: "http"}},
		Results: []model.Result{{Target: "api", At: at, OK: true, State: "UP", DurationMS: 12}},
	}
	if _, err := v.Ingest(context.Background(), b); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
}

func TestStatusHealthy(t *testing.T) {
	v, now := newTestViewer(t, Config{StaleAfter: 30 * time.Second, ExpectedProbers: []string{"prober-a"}})
	ingestUp(t, v, *now)

	st, err := v.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(st.Targets))
	}
	if st.Targets[0].State != "UP" || st.Targets[0].Stale {
		t.Fatalf("target = %+v, want UP and not stale", st.Targets[0])
	}
	if st.Summary.Up != 1 || st.Summary.Stale != 0 {
		t.Fatalf("summary = %+v, want up=1 stale=0", st.Summary)
	}
	if len(st.Probers) != 1 || st.Probers[0].State != "HEALTHY" {
		t.Fatalf("probers = %+v, want prober-a HEALTHY", st.Probers)
	}
}

// The core promise of the push model: a silent prober makes its targets STALE,
// never DOWN, because the last observation is still the last known truth.
func TestSilentProberMakesTargetsStale(t *testing.T) {
	v, now := newTestViewer(t, Config{StaleAfter: 30 * time.Second})
	ingestUp(t, v, *now)

	// Two minutes later the prober has not reported.
	*now = now.Add(2 * time.Minute)

	st, err := v.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Targets[0].State != "STALE" {
		t.Fatalf("state = %q, want STALE (a silent prober must not be reported DOWN)", st.Targets[0].State)
	}
	if !st.Targets[0].Stale {
		t.Fatal("stale flag not set")
	}
	if st.Targets[0].Last == nil || !st.Targets[0].Last.OK {
		t.Fatalf("last result lost: %+v", st.Targets[0].Last)
	}
	if st.Summary.Stale != 1 || st.Summary.Up != 0 {
		t.Fatalf("summary = %+v, want stale=1 up=0", st.Summary)
	}
	if st.Probers[0].State != "STALE" {
		t.Fatalf("prober state = %q, want STALE", st.Probers[0].State)
	}
}

func TestExpectedProberThatNeverReportedIsMissing(t *testing.T) {
	v, _ := newTestViewer(t, Config{
		StaleAfter:      30 * time.Second,
		ExpectedProbers: []string{"prober-a", "prober-b"},
	})
	ingestUp(t, v, time.Now().UTC())

	st, err := v.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st.Probers) != 2 {
		t.Fatalf("probers = %d, want 2 (one reporting, one expected)", len(st.Probers))
	}
	if st.Probers[1].Name != "prober-b" || st.Probers[1].State != "MISSING" {
		t.Fatalf("second prober = %+v, want prober-b MISSING", st.Probers[1])
	}
	if st.Summary.ProbersMissing != 1 {
		t.Fatalf("summary = %+v, want probers_missing=1", st.Summary)
	}
}

func TestHistoryDefaultsToOneHour(t *testing.T) {
	v, now := newTestViewer(t, Config{})
	ingestUp(t, v, now.Add(-30*time.Minute))
	ingestUp(t, v, now.Add(-2*time.Hour))

	results, err := v.History(context.Background(), "prober-a", "api", 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (default window is 1h)", len(results))
	}
}

func TestMaintenanceIsNotCountedAsDown(t *testing.T) {
	v, now := newTestViewer(t, Config{StaleAfter: time.Minute})

	b := model.Batch{
		Prober:  model.ProberInfo{Name: "prober-a", Version: "test"},
		Targets: []model.TargetSpec{{Name: "api", Group: "web", Type: "http"}},
		Results: []model.Result{{
			Target: "api", At: *now, OK: false, State: "DOWN", ErrorClass: "CONNECT_REFUSED",
			Error: "connection refused", Maintenance: true,
		}},
	}
	if _, err := v.Ingest(context.Background(), b); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	st, err := v.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := st.Targets[0].State; got != "MAINTENANCE" {
		t.Fatalf("state = %q, want MAINTENANCE (a planned outage is not a failure)", got)
	}
	if !st.Targets[0].Maintenance {
		t.Fatal("target was not marked as in maintenance")
	}
	if st.Summary.Down != 0 {
		t.Fatalf("summary down = %d, want 0 during maintenance", st.Summary.Down)
	}
	if st.Summary.Maintenance != 1 {
		t.Fatalf("summary maintenance = %d, want 1", st.Summary.Maintenance)
	}
	// The failure is still recorded, so nothing is hidden from history.
	if last := st.Targets[0].Last; last == nil || last.OK || !last.Maintenance {
		t.Fatalf("last result = %+v, want a recorded failure flagged as maintenance", last)
	}
}

func TestStaleOutranksMaintenance(t *testing.T) {
	v, now := newTestViewer(t, Config{StaleAfter: 10 * time.Second})

	b := model.Batch{
		Prober:  model.ProberInfo{Name: "prober-a", Version: "test"},
		Targets: []model.TargetSpec{{Name: "api"}},
		Results: []model.Result{{Target: "api", At: *now, OK: false, State: "DOWN", Maintenance: true}},
	}
	if _, err := v.Ingest(context.Background(), b); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// The prober has gone silent: freshness is the more important fact, so the
	// target is STALE rather than "planned maintenance, all fine".
	*now = now.Add(5 * time.Minute)
	st, err := v.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := st.Targets[0].State; got != "STALE" {
		t.Fatalf("state = %q, want STALE to outrank MAINTENANCE", got)
	}
}
