package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPKeepAlivePhases(t *testing.T) {
	const latency = 15 * time.Millisecond
	var requests atomic.Int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 3 {
			<-r.Context().Done() // timeout waiting for headers on a reused connection
			return
		}
		time.Sleep(latency)
		fmt.Fprint(w, "ok")
	}))
	defer ts.Close()

	h, err := NewHTTP(target("api", ts.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	// Trust the test certificate but keep the actual probe transport.
	h.client.Transport.(*http.Transport).TLSClientConfig.RootCAs = ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	for i := 0; i < 3; i++ {
		var acquisitions atomic.Int32
		var reused atomic.Bool
		ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				acquisitions.Add(1)
				reused.Store(info.Reused)
			},
		})
		if i == 2 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
			defer cancel()
		}
		res := h.Probe(ctx)
		if acquisitions.Load() != 1 || reused.Load() != (i != 0) {
			t.Fatalf("probe %d: acquisitions = %d, reused = %t (%s)", i, acquisitions.Load(), reused.Load(), res.Error)
		}
		if i == 2 {
			if res.ErrorClass != ClassHTTPTimeout {
				t.Errorf("reused connection: got %s, want HTTP_TIMEOUT (%s)", res.ErrorClass, res.Error)
			}
			continue
		}
		if !res.OK {
			t.Fatalf("probe %d: %s", i, res.Error)
		}
		for _, phase := range []Phase{PhaseDNS, PhaseConnect, PhaseTLS, PhaseTTFB, PhaseTotal} {
			v, ok := res.PhaseMS[phase]
			if !ok || v < 0 || v > res.DurationMS {
				t.Errorf("probe %d: invalid %s phase: %v (total %dms)", i, phase, res.PhaseMS, res.DurationMS)
			}
		}
		if v := res.PhaseMS[PhaseTTFB]; v < latency.Milliseconds() {
			t.Errorf("probe %d: TTFB %dms excludes server delay", i, v)
		}
		if i == 1 && (res.PhaseMS[PhaseDNS] != 0 || res.PhaseMS[PhaseConnect] != 0 || res.PhaseMS[PhaseTLS] != 0) {
			t.Errorf("reused connection includes setup time: %v", res.PhaseMS)
		}
	}
}

func TestHTTPBodyTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res := mustProbe(t, target("api", ts.URL, nil)).Probe(ctx)
	if res.StatusCode != http.StatusOK || res.ErrorClass != ClassHTTPTimeout {
		t.Errorf("body timeout: status %d, class %s (%s)", res.StatusCode, res.ErrorClass, res.Error)
	}
}

func TestHTTPConcurrentTraceCallbacks(t *testing.T) {
	// A speculative dial can outlive connection acquisition and even Probe.
	// Exercise parallel dial callbacks while the probe takes its snapshot.
	var phases httpPhases
	trace := phases.trace()
	trace.GetConn("host:443")
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, addr := range []string{"[::1]:443", "127.0.0.1:443"} {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			<-start
			trace.DNSStart(httptrace.DNSStartInfo{})
			trace.DNSDone(httptrace.DNSDoneInfo{})
			trace.ConnectStart("tcp", addr)
			trace.ConnectDone("tcp", addr, nil)
			trace.TLSHandshakeStart()
			trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
		}(addr)
	}
	close(start)
	trace.GotConn(httptrace.GotConnInfo{Reused: true})
	trace.GotFirstResponseByte()
	before := phases.snapshot()
	wg.Wait()
	if after := phases.snapshot(); before != after || after.dns != 0 || after.connect != 0 || after.tls != 0 {
		t.Fatalf("dial callbacks changed reused connection timings: before=%+v after=%+v", before, after)
	}
}
