// Package model defines the wire contract between probers and the viewer.
//
// It is deliberately separate from internal/probe: the probe package owns how a
// target is measured, this package owns what crosses the network and what the
// store persists.
package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProberInfo identifies the prober that produced a batch.
type ProberInfo struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// TargetSpec is the configuration of one target as the prober sees it.
type TargetSpec struct {
	Name  string `json:"name"`
	Group string `json:"group"`
	Type  string `json:"type"`
}

// Result is one probe execution.
//
// State carries the prober's damped state (UP/DOWN/UNKNOWN) after this
// observation, so flap damping lives in exactly one place: the prober.
type Result struct {
	Target     string           `json:"target"`
	At         time.Time        `json:"at"`
	OK         bool             `json:"ok"`
	State      string           `json:"state,omitempty"`
	DurationMS int64            `json:"duration_ms"`
	PhaseMS    map[string]int64 `json:"phase_ms,omitempty"`
	StatusCode int              `json:"status_code,omitempty"`
	ErrorClass string           `json:"error_class"`
	Error      string           `json:"error,omitempty"`
	TLSExpiry  *time.Time       `json:"tls_expiry,omitempty"`
	// Maintenance marks a result taken while the target was inside one of its
	// maintenance windows. The raw OK stays truthful; the flag lets the viewer
	// stop counting the target as DOWN without rewriting history.
	Maintenance bool `json:"maintenance,omitempty"`
}

// Batch is one push from a prober: the full target list plus new results.
type Batch struct {
	Prober     ProberInfo   `json:"prober"`
	ConfigHash string       `json:"config_hash,omitempty"`
	Targets    []TargetSpec `json:"targets,omitempty"`
	Results    []Result     `json:"results"`
}

// MaxBatchResults bounds a single push, so one bad client cannot exhaust memory.
const MaxBatchResults = 5000

// Validate rejects a batch the viewer must not store.
func (b Batch) Validate() error {
	var errs []error
	if strings.TrimSpace(b.Prober.Name) == "" {
		errs = append(errs, errors.New("prober.name is required"))
	}
	if len(b.Results) == 0 {
		errs = append(errs, errors.New("results must not be empty"))
	}
	if len(b.Results) > MaxBatchResults {
		errs = append(errs, fmt.Errorf("results must not exceed %d per batch", MaxBatchResults))
	}

	seen := make(map[string]struct{}, len(b.Results))
	for i, r := range b.Results {
		if strings.TrimSpace(r.Target) == "" {
			errs = append(errs, fmt.Errorf("results[%d]: target is required", i))
			continue
		}
		if r.At.IsZero() {
			errs = append(errs, fmt.Errorf("results[%d] (%s): at is required", i, r.Target))
		}
		key := r.Target + "\x00" + fmt.Sprint(r.At.UnixNano())
		if _, dup := seen[key]; dup {
			errs = append(errs, fmt.Errorf("results[%d] (%s): duplicate (target, at) in the batch", i, r.Target))
		}
		seen[key] = struct{}{}
	}

	return errors.Join(errs...)
}

// KnownStates are the states a prober may report.
var KnownStates = map[string]struct{}{
	"UNKNOWN": {},
	"UP":      {},
	"DOWN":    {},
}

// NormalizeState maps anything unexpected to UNKNOWN rather than trusting input.
func NormalizeState(s string) string {
	if _, ok := KnownStates[s]; ok {
		return s
	}
	return "UNKNOWN"
}
