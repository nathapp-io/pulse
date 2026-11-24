package main

import (
	"testing"
	"time"

	"pulse/internal/config"
	"pulse/internal/probe"
	"pulse/internal/state"
)

const proberConfig = `
prober:
  name: p
defaults:
  interval: 2s
  timeout: 1s
probes:
  - name: a
    type: http
    url: http://127.0.0.1:1/ok
`

func parseConfig(t *testing.T, yaml string) *config.File {
	t.Helper()
	f, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func TestReloadPreservesUnchangedTargets(t *testing.T) {
	first, err := buildTargets(parseConfig(t, proberConfig), nil)
	if err != nil {
		t.Fatalf("buildTargets: %v", err)
	}

	// Put the target into DOWN with some recorded history.
	now := time.Now()
	first["a"].machine.Observe(false, now)
	first["a"].machine.Observe(false, now)
	first["a"].buckets.Record(false, now)

	// Reload with identical configuration.
	second, err := buildTargets(parseConfig(t, proberConfig), first)
	if err != nil {
		t.Fatalf("buildTargets on reload: %v", err)
	}
	if second["a"] != first["a"] {
		t.Fatal("an unchanged target must reuse its runtime (state and uptime would otherwise reset)")
	}

	got, _ := second["a"].machine.Snapshot()
	if got != state.Down {
		t.Fatalf("state after reload = %s, want %s", got, state.Down)
	}
	if ratio := second["a"].buckets.Ratio(time.Hour); ratio != 0 {
		t.Fatalf("uptime ratio after reload = %v, want the pre-reload history preserved", ratio)
	}
}

func TestReloadRebuildsChangedTargets(t *testing.T) {
	first, err := buildTargets(parseConfig(t, proberConfig), nil)
	if err != nil {
		t.Fatalf("buildTargets: %v", err)
	}
	first["a"].machine.Observe(false, time.Now())
	first["a"].machine.Observe(false, time.Now())

	changed := `
prober:
  name: p
defaults:
  interval: 5s
  timeout: 1s
probes:
  - name: a
    type: http
    url: http://127.0.0.1:1/ok
`
	second, err := buildTargets(parseConfig(t, changed), first)
	if err != nil {
		t.Fatalf("buildTargets: %v", err)
	}
	if second["a"] == first["a"] {
		t.Fatal("a target whose configuration changed must be rebuilt")
	}
	if got, _ := second["a"].machine.Snapshot(); got != state.Unknown {
		t.Fatalf("rebuilt target state = %s, want %s", got, state.Unknown)
	}
}

func TestBuildTargetsRejectsInvalidProbe(t *testing.T) {
	// icmp is accepted by config validation but has no prober yet.
	cfg := parseConfig(t, `
prober:
  name: p
probes:
  - name: gateway
    type: icmp
    address: 192.0.2.1
`)
	if _, err := buildTargets(cfg, nil); err == nil {
		t.Fatal("expected buildTargets to reject an unimplemented probe type")
	}
}

// A result taken inside a declared window must travel to the viewer flagged, while
// the raw failure stays a failure: the schedule is a presentation concern, history
// is not.
func TestToModelResultCarriesMaintenance(t *testing.T) {
	res := probe.Result{
		Target:     "api",
		Group:      "web",
		Type:       "http",
		At:         time.Unix(1000, 0).UTC(),
		OK:         false,
		ErrorClass: probe.ClassConnectRefused,
		Error:      "connection refused",
		DurationMS: 3,
	}

	planned := toModelResult(res, state.Down, true)
	if !planned.Maintenance {
		t.Fatal("maintenance flag was not carried to the viewer")
	}
	if planned.OK {
		t.Fatal("maintenance rewrote the raw result to OK")
	}
	if planned.State != string(state.Down) {
		t.Fatalf("state = %q, want the damped state %q", planned.State, state.Down)
	}

	normal := toModelResult(res, state.Down, false)
	if normal.Maintenance {
		t.Fatal("a result outside a window was flagged as maintenance")
	}
}

func TestInMaintenanceWindow(t *testing.T) {
	p := config.Probe{
		Name: "api",
		Maintenance: []config.Window{{
			From: time.Unix(1000, 0),
			To:   time.Unix(2000, 0),
		}},
	}
	if p.InMaintenance(time.Unix(500, 0)) {
		t.Fatal("time before the window counted as maintenance")
	}
	if !p.InMaintenance(time.Unix(1500, 0)) {
		t.Fatal("time inside the window did not count as maintenance")
	}
	if p.InMaintenance(time.Unix(2500, 0)) {
		t.Fatal("time after the window counted as maintenance")
	}
}
