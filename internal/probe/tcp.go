package probe

import (
	"context"
	"net"
	"time"

	"pulse/internal/config"
)

// TCP probes one host:port, which answers "is the port reachable" without
// needing anything to speak a protocol on the other side.
type TCP struct {
	cfg *config.Probe
}

// NewTCP builds a TCP prober.
func NewTCP(p *config.Probe) *TCP { return &TCP{cfg: p} }

// Target implements Prober.
func (t *TCP) Target() string { return t.cfg.Name }

// Probe implements Prober.
func (t *TCP) Probe(ctx context.Context) Result {
	p := t.cfg
	start := time.Now()
	res := Result{
		Target:  p.Name,
		Group:   p.Group,
		Type:    p.Type,
		At:      start,
		PhaseMS: make(map[Phase]int64, 2),
	}

	dialCtx, cancel := context.WithTimeout(ctx, p.Timeout.D())
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(dialCtx, "tcp", p.Address)
	if err != nil {
		return fail(res, start, Classify(err, PhaseConnect), err)
	}
	_ = conn.Close()

	res.DurationMS = time.Since(start).Milliseconds()
	res.PhaseMS[PhaseConnect] = res.DurationMS
	res.PhaseMS[PhaseTotal] = res.DurationMS
	res.OK = true
	res.ErrorClass = ClassOK
	return res
}
