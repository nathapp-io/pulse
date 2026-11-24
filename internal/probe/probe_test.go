package probe

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pulse/internal/config"
)

func target(name, url string, mut func(*config.Probe)) *config.Probe {
	p := &config.Probe{
		Name:         name,
		Group:        "test",
		Type:         "http",
		URL:          url,
		Method:       "GET",
		Interval:     config.Duration(30 * time.Second),
		Timeout:      config.Duration(2 * time.Second),
		ConfirmDown:  2,
		ConfirmUp:    1,
		MaxBodyBytes: 64 << 10,
	}
	if mut != nil {
		mut(p)
	}
	return p
}

func mustProbe(t *testing.T, p *config.Probe) Prober {
	t.Helper()
	pr, err := New(p)
	if err != nil {
		t.Fatalf("New(%s): %v", p.Name, err)
	}
	return pr
}

func TestHTTPOK(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "healthy")
	}))
	defer ts.Close()

	res := mustProbe(t, target("api", ts.URL, nil)).Probe(context.Background())
	if !res.OK {
		t.Fatalf("want OK, got %s (%s)", res.ErrorClass, res.Error)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if res.ErrorClass != ClassOK {
		t.Errorf("class = %s, want %s", res.ErrorClass, ClassOK)
	}
	if got := res.PhaseMS[PhaseTotal]; got != res.DurationMS {
		t.Errorf("total phase = %d, duration = %d", got, res.DurationMS)
	}
}

func TestHTTPStatusClassification(t *testing.T) {
	tests := []struct {
		name   string
		code   int
		expect []int
		want   ErrorClass
	}{
		{name: "500 without expectation", code: 500, want: ClassHTTPStatus},
		{name: "204 without expectation", code: 204, want: ClassOK},
		{name: "200 when 204 expected", code: 200, expect: []int{204}, want: ClassHTTPStatus},
		{name: "204 when 204 expected", code: 204, expect: []int{200, 204}, want: ClassOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer ts.Close()

			p := target("api", ts.URL, func(p *config.Probe) { p.Expect.Status = tc.expect })
			res := mustProbe(t, p).Probe(context.Background())
			if res.ErrorClass != tc.want {
				t.Fatalf("class = %s, want %s (error: %s)", res.ErrorClass, tc.want, res.Error)
			}
		})
	}
}

func TestHTTPBodyRegex(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer ts.Close()

	p := target("api", ts.URL, func(p *config.Probe) { p.Expect.BodyRegex = `"status":"ok"` })
	if res := mustProbe(t, p).Probe(context.Background()); !res.OK {
		t.Fatalf("matching body should pass, got %s (%s)", res.ErrorClass, res.Error)
	}

	p = target("api", ts.URL, func(p *config.Probe) { p.Expect.BodyRegex = `"status":"down"` })
	res := mustProbe(t, p).Probe(context.Background())
	if res.ErrorClass != ClassAssertFail {
		t.Fatalf("class = %s, want %s", res.ErrorClass, ClassAssertFail)
	}
}

func TestHTTPJSONPath(t *testing.T) {
	const body = `{"status":"ok","db":{"up":true},"checks":[{"name":"a","n":3}]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer ts.Close()

	tests := []struct {
		name string
		jp   config.JSONPathCheck
		want ErrorClass
	}{
		{name: "string equals", jp: config.JSONPathCheck{Path: "$.status", Equals: "ok"}, want: ClassOK},
		{name: "string mismatch", jp: config.JSONPathCheck{Path: "$.status", Equals: "down"}, want: ClassAssertFail},
		{name: "nested bool", jp: config.JSONPathCheck{Path: "$.db.up", Equals: "true"}, want: ClassOK},
		{name: "array index number", jp: config.JSONPathCheck{Path: "$.checks.0.n", Equals: "3"}, want: ClassOK},
		{name: "array out of range", jp: config.JSONPathCheck{Path: "$.checks.1.n", Equals: "3"}, want: ClassAssertFail},
		{name: "missing key", jp: config.JSONPathCheck{Path: "$.nope", Equals: "x"}, want: ClassAssertFail},
		{name: "contains", jp: config.JSONPathCheck{Path: "$.status", Contains: "o"}, want: ClassOK},
		{name: "contains mismatch", jp: config.JSONPathCheck{Path: "$.status", Contains: "zzz"}, want: ClassAssertFail},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := target("api", ts.URL, func(p *config.Probe) {
				jp := tc.jp
				p.Expect.JSONPath = &jp
			})
			res := mustProbe(t, p).Probe(context.Background())
			if res.ErrorClass != tc.want {
				t.Fatalf("class = %s, want %s (error: %s)", res.ErrorClass, tc.want, res.Error)
			}
		})
	}
}

func TestHTTPHeaderAssertion(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Health", "green")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	p := target("api", ts.URL, func(p *config.Probe) {
		p.Expect.Header = map[string]string{"X-Health": "^green$"}
	})
	if res := mustProbe(t, p).Probe(context.Background()); !res.OK {
		t.Fatalf("matching header should pass, got %s (%s)", res.ErrorClass, res.Error)
	}

	p = target("api", ts.URL, func(p *config.Probe) {
		p.Expect.Header = map[string]string{"X-Missing": ".*"}
	})
	if res := mustProbe(t, p).Probe(context.Background()); res.ErrorClass != ClassAssertFail {
		t.Fatalf("missing header class = %s, want %s", res.ErrorClass, ClassAssertFail)
	}
}

func TestHTTPTimeoutIsHTTPSlowNotConnect(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, "late")
	}))
	defer ts.Close()

	p := target("api", ts.URL, func(p *config.Probe) {
		p.Timeout = config.Duration(50 * time.Millisecond)
	})
	res := mustProbe(t, p).Probe(context.Background())
	if res.ErrorClass != ClassHTTPTimeout {
		t.Fatalf("class = %s, want %s (a connected server is slow, not unreachable)", res.ErrorClass, ClassHTTPTimeout)
	}
}

func TestHTTPConnectRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	res := mustProbe(t, target("api", "http://"+addr+"/", nil)).Probe(context.Background())
	if res.ErrorClass != ClassConnectRefused {
		t.Fatalf("class = %s, want %s (error: %s)", res.ErrorClass, ClassConnectRefused, res.Error)
	}
}

func TestHTTPFollowRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ok", http.StatusFound)
	})
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "done")
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	follow := true
	p := target("api", ts.URL+"/redirect", func(p *config.Probe) { p.FollowRedirects = &follow })
	if res := mustProbe(t, p).Probe(context.Background()); !res.OK {
		t.Fatalf("following redirects should pass, got %s (%s)", res.ErrorClass, res.Error)
	}

	noFollow := false
	p = target("api", ts.URL+"/redirect", func(p *config.Probe) { p.FollowRedirects = &noFollow })
	res := mustProbe(t, p).Probe(context.Background())
	if res.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", res.StatusCode)
	}
	if res.ErrorClass != ClassHTTPStatus {
		t.Fatalf("class = %s, want %s", res.ErrorClass, ClassHTTPStatus)
	}
}

func TestTLSExpiringWarning(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer ts.Close()

	p := target("api", ts.URL, func(p *config.Probe) {
		// httptest's certificate is valid for ~60 more years, so the threshold
		// has to exceed that to exercise the warning path.
		p.Expect.TLSExpiryMin = config.Duration(100 * 365 * 24 * time.Hour)
	})
	h, err := NewHTTP(p)
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	h.client = ts.Client() // trust the test certificate

	res := h.Probe(context.Background())
	if res.ErrorClass != ClassTLSExpiring {
		t.Fatalf("class = %s, want %s (error: %s)", res.ErrorClass, ClassTLSExpiring, res.Error)
	}
	if res.TLSExpiry.IsZero() {
		t.Error("TLSExpiry should be populated for an https target")
	}
}

func TestHTTPBodyTooLarge(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 4096))
	}))
	defer ts.Close()

	p := target("api", ts.URL, func(p *config.Probe) { p.MaxBodyBytes = 128 })
	res := mustProbe(t, p).Probe(context.Background())
	if res.ErrorClass != ClassBodyTooLarge {
		t.Fatalf("class = %s, want %s", res.ErrorClass, ClassBodyTooLarge)
	}
}

func TestTCP(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()

	p := &config.Probe{
		Name:       "db",
		Group:      "test",
		Type:       "tcp",
		Address:    addr,
		Interval:   config.Duration(30 * time.Second),
		Timeout:    config.Duration(time.Second),
		ConfirmDown: 2,
		ConfirmUp:   1,
	}
	if res := mustProbe(t, p).Probe(context.Background()); !res.OK {
		t.Fatalf("open port should pass, got %s (%s)", res.ErrorClass, res.Error)
	}

	_ = l.Close()
	if res := mustProbe(t, p).Probe(context.Background()); res.ErrorClass != ClassConnectRefused {
		t.Fatalf("class = %s, want %s", res.ErrorClass, ClassConnectRefused)
	}
}
