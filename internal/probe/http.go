package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"pulse/internal/config"
)

// HTTP probes one http(s) target, with optional assertions on the response.
type HTTP struct {
	cfg       *config.Probe
	client    *http.Client
	statusSet map[int]struct{}
	bodyRe    *regexp.Regexp
	headerRe  map[string]*regexp.Regexp
}

// NewHTTP compiles a target's assertions once, so a bad regex fails at startup
// instead of on every probe.
func NewHTTP(p *config.Probe) (*HTTP, error) {
	h := &HTTP{
		cfg:       p,
		statusSet: make(map[int]struct{}, len(p.Expect.Status)),
	}
	for _, s := range p.Expect.Status {
		h.statusSet[s] = struct{}{}
	}
	if p.Expect.BodyRegex != "" {
		re, err := regexp.Compile(p.Expect.BodyRegex)
		if err != nil {
			return nil, fmt.Errorf("%s: expect.body_regex: %w", p.Name, err)
		}
		h.bodyRe = re
	}
	if len(p.Expect.Header) > 0 {
		h.headerRe = make(map[string]*regexp.Regexp, len(p.Expect.Header))
		for name, expr := range p.Expect.Header {
			re, err := regexp.Compile(expr)
			if err != nil {
				return nil, fmt.Errorf("%s: expect.header[%s]: %w", p.Name, name, err)
			}
			h.headerRe[http.CanonicalHeaderKey(name)] = re
		}
	}

	transport := &http.Transport{
		// Probes must go direct: honouring HTTP(S)_PROXY would report "up"
		// for a target that is unreachable from this host, and would blame
		// the proxy for the proxy's own failures.
		Proxy:               nil,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: p.Timeout.D(),
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}

	h.client = &http.Client{Transport: transport}
	if p.FollowRedirects != nil && !*p.FollowRedirects {
		h.client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return h, nil
}

// Target implements Prober.
func (h *HTTP) Target() string { return h.cfg.Name }

// Probe implements Prober.
func (h *HTTP) Probe(ctx context.Context) Result {
	p := h.cfg
	start := time.Now()
	res := Result{
		Target:  p.Name,
		Group:   p.Group,
		Type:    p.Type,
		At:      start,
		PhaseMS: make(map[Phase]int64, 5),
	}

	ctx, cancel := context.WithTimeout(ctx, p.Timeout.D())
	defer cancel()

	phases := new(httpPhases)

	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, phases.trace()), p.Method, p.URL, nil)
	if err != nil {
		return fail(res, start, ClassError, err)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "pulse-prober/0.1")
	}

	resp, err := h.client.Do(req)
	timing := phases.snapshot()
	if err != nil {
		phase := PhaseConnect
		if !timing.gotConn.IsZero() {
			phase = PhaseTTFB
		}
		return fail(res, start, Classify(err, phase), err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, p.MaxBodyBytes+1))

	total := time.Since(start)
	res.DurationMS = total.Milliseconds()
	res.StatusCode = resp.StatusCode
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		res.TLSExpiry = resp.TLS.PeerCertificates[0].NotAfter
	}

	res.PhaseMS[PhaseDNS] = timing.dns.Milliseconds()
	res.PhaseMS[PhaseConnect] = timing.connect.Milliseconds()
	res.PhaseMS[PhaseTLS] = timing.tls.Milliseconds()
	if !timing.firstByte.IsZero() && !timing.gotConn.IsZero() {
		res.PhaseMS[PhaseTTFB] = timing.firstByte.Sub(timing.gotConn).Milliseconds()
	}
	res.PhaseMS[PhaseTotal] = res.DurationMS

	switch {
	case readErr != nil:
		res.ErrorClass = Classify(readErr, PhaseTotal)
		res.Error = "read body: " + readErr.Error()
	case int64(len(body)) > p.MaxBodyBytes:
		res.ErrorClass = ClassBodyTooLarge
		res.Error = fmt.Sprintf("body exceeded %d bytes", p.MaxBodyBytes)
	default:
		res.ErrorClass, res.Error = h.evaluate(resp.StatusCode, resp.Header, body, res.TLSExpiry, start)
	}
	res.OK = res.ErrorClass == ClassOK
	return res
}

// httptrace hooks may overlap (e.g. Happy Eyeballs) and may run after Do
// returns. Copy the timings under the same lock used by the hooks; callbacks
// never touch the Result itself.
type httpTimings struct {
	dns, connect, tls  time.Duration
	gotConn, firstByte time.Time
}

type httpPhases struct {
	mu                 sync.Mutex
	timing             httpTimings
	dnsStart, tlsStart time.Time
	connectStarts      map[[2]string]time.Time
}

func (p *httpPhases) snapshot() httpTimings {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.timing
}

func (p *httpPhases) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) {
			// A redirect or a transport retry starts a new acquisition.
			p.mu.Lock()
			defer p.mu.Unlock()
			p.timing = httpTimings{}
			p.dnsStart, p.tlsStart = time.Time{}, time.Time{}
			p.connectStarts = nil
		},
		DNSStart: func(httptrace.DNSStartInfo) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.dnsStart = time.Now()
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.timing.gotConn.IsZero() && !p.dnsStart.IsZero() {
				p.timing.dns = time.Since(p.dnsStart)
			}
		},
		ConnectStart: func(network, addr string) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.connectStarts == nil {
				p.connectStarts = make(map[[2]string]time.Time)
			}
			p.connectStarts[[2]string{network, addr}] = time.Now()
		},
		ConnectDone: func(network, addr string, err error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			key := [2]string{network, addr}
			if start, ok := p.connectStarts[key]; ok && err == nil && p.timing.gotConn.IsZero() {
				p.timing.connect = time.Since(start)
			}
			delete(p.connectStarts, key)
		},
		TLSHandshakeStart: func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.tlsStart = time.Now()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if err == nil && p.timing.gotConn.IsZero() && !p.tlsStart.IsZero() {
				p.timing.tls = time.Since(p.tlsStart)
			}
		},
		GotConn: func(info httptrace.GotConnInfo) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if info.Reused {
				// A speculative dial may have raced the idle connection.
				p.timing = httpTimings{}
			}
			p.timing.gotConn = time.Now()
		},
		GotFirstResponseByte: func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.timing.firstByte.IsZero() && !p.timing.gotConn.IsZero() {
				p.timing.firstByte = time.Now()
			}
		},
	}
}

func fail(res Result, start time.Time, class ErrorClass, err error) Result {
	res.DurationMS = time.Since(start).Milliseconds()
	if res.PhaseMS == nil {
		res.PhaseMS = make(map[Phase]int64, 1)
	}
	res.PhaseMS[PhaseTotal] = res.DurationMS
	res.ErrorClass = class
	res.Error = err.Error()
	res.OK = false
	return res
}

// evaluate applies assertions in the order that produces the most useful message:
// status first (the usual cause), then body, JSON, headers, then TLS lifetime.
func (h *HTTP) evaluate(status int, header http.Header, body []byte, tlsExpiry time.Time, now time.Time) (ErrorClass, string) {
	p := h.cfg

	if len(h.statusSet) > 0 {
		if _, ok := h.statusSet[status]; !ok {
			return ClassHTTPStatus, fmt.Sprintf("status %d not in %v", status, p.Expect.Status)
		}
	} else if status < 200 || status > 299 {
		return ClassHTTPStatus, fmt.Sprintf("status %d is not 2xx", status)
	}

	if h.bodyRe != nil && !h.bodyRe.Match(body) {
		return ClassAssertFail, fmt.Sprintf("body does not match /%s/", p.Expect.BodyRegex)
	}

	if jp := p.Expect.JSONPath; jp != nil {
		got, found, err := lookupJSONPath(body, jp.Path)
		switch {
		case err != nil:
			return ClassAssertFail, fmt.Sprintf("json_path %s: %v", jp.Path, err)
		case !found:
			return ClassAssertFail, fmt.Sprintf("json_path %s not found", jp.Path)
		case jp.Equals != "" && got != jp.Equals:
			return ClassAssertFail, fmt.Sprintf("json_path %s = %q, want %q", jp.Path, got, jp.Equals)
		case jp.Contains != "" && !strings.Contains(got, jp.Contains):
			return ClassAssertFail, fmt.Sprintf("json_path %s = %q, does not contain %q", jp.Path, got, jp.Contains)
		}
	}

	for name, re := range h.headerRe {
		v := header.Get(name)
		if v == "" {
			return ClassAssertFail, fmt.Sprintf("header %s is missing", name)
		}
		if !re.MatchString(v) {
			return ClassAssertFail, fmt.Sprintf("header %s = %q does not match /%s/", name, v, re.String())
		}
	}

	if min := p.Expect.TLSExpiryMin.D(); min > 0 && !tlsExpiry.IsZero() {
		if tlsExpiry.Sub(now) < min {
			return ClassTLSExpiring, fmt.Sprintf("certificate expires %s", tlsExpiry.UTC().Format(time.RFC3339))
		}
	}

	return ClassOK, ""
}

// lookupJSONPath resolves a dotted subset such as "$.db.status" or "$.checks.0.ok".
// It is deliberately not full JSONPath: no filters, wildcards or recursion.
func lookupJSONPath(body []byte, path string) (string, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	var root any
	if err := dec.Decode(&root); err != nil {
		return "", false, fmt.Errorf("invalid json: %w", err)
	}

	cur := root
	for _, seg := range strings.Split(strings.TrimPrefix(path, "$."), ".") {
		if seg == "" {
			continue
		}
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return "", false, nil
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil {
				return "", false, fmt.Errorf("segment %q is not an array index", seg)
			}
			if idx < 0 || idx >= len(node) {
				return "", false, nil
			}
			cur = node[idx]
		default:
			return "", false, nil
		}
	}

	switch v := cur.(type) {
	case nil:
		return "", true, nil
	case string:
		return v, true, nil
	case json.Number:
		return v.String(), true, nil
	case bool:
		return strconv.FormatBool(v), true, nil
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return "", false, err
		}
		return string(encoded), true, nil
	}
}
