// Package probe executes a single health check against one target and classifies
// the outcome. It performs no scheduling, storage or reporting.
package probe

import (
	"context"
	"fmt"
	"time"

	"pulse/internal/config"
)

// Phase is one measured segment of a probe.
type Phase string

// Probe phases. Only the phases a given probe type can measure are populated.
const (
	PhaseDNS     Phase = "dns"
	PhaseConnect Phase = "connect"
	PhaseTLS     Phase = "tls"
	PhaseTTFB    Phase = "ttfb"
	PhaseTotal   Phase = "total"
)

// ErrorClass explains *why* a probe failed, which is what makes the UI useful.
type ErrorClass string

// Error classes.
const (
	ClassOK             ErrorClass = "OK"
	ClassDNSFail        ErrorClass = "DNS_FAIL"
	ClassConnectRefused ErrorClass = "CONNECT_REFUSED"
	ClassConnectTimeout ErrorClass = "CONNECT_TIMEOUT"
	ClassTLSHandshake   ErrorClass = "TLS_HANDSHAKE"
	ClassTLSExpired     ErrorClass = "TLS_EXPIRED"
	ClassTLSExpiring    ErrorClass = "TLS_EXPIRING"
	ClassHTTPTimeout    ErrorClass = "HTTP_TIMEOUT"
	ClassHTTPStatus     ErrorClass = "HTTP_STATUS"
	ClassAssertFail     ErrorClass = "ASSERT_FAIL"
	ClassBodyTooLarge   ErrorClass = "BODY_TOO_LARGE"
	ClassError          ErrorClass = "ERROR"
)

// Result is the outcome of one probe execution.
type Result struct {
	Target     string
	Group      string
	Type       string
	At         time.Time
	OK         bool
	DurationMS int64
	PhaseMS    map[Phase]int64
	StatusCode int
	ErrorClass ErrorClass
	Error      string
	TLSExpiry  time.Time
}

// Prober executes repeated probes against a single configured target.
type Prober interface {
	// Probe runs one check. The context carries the caller's lifetime; the
	// per-target timeout is applied inside.
	Probe(ctx context.Context) Result
	// Target returns the configured target name.
	Target() string
}

// New builds the prober for a configured target.
func New(p *config.Probe) (Prober, error) {
	switch p.Type {
	case "http":
		return NewHTTP(p)
	case "tcp":
		return NewTCP(p), nil
	case "icmp":
		return nil, fmt.Errorf("%s: icmp probes are not implemented yet", p.Name)
	default:
		return nil, fmt.Errorf("%s: unknown probe type %q", p.Name, p.Type)
	}
}
