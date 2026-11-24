package config

import (
	"strings"
	"testing"
	"time"
)

const minimal = `
prober:
  name: prober-lan-a
probes:
  - name: api
    type: http
    url: https://example.com/health
`

func TestParseAppliesDefaults(t *testing.T) {
	f, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got, want := f.Probes[0].Interval.D(), 30*time.Second; got != want {
		t.Errorf("interval = %s, want %s", got, want)
	}
	if got, want := f.Probes[0].Timeout.D(), 5*time.Second; got != want {
		t.Errorf("timeout = %s, want %s", got, want)
	}
	if got, want := f.Probes[0].ConfirmDown, 2; got != want {
		t.Errorf("confirm_down = %d, want %d", got, want)
	}
	if got, want := f.Probes[0].Group, "default"; got != want {
		t.Errorf("group = %q, want %q", got, want)
	}
	if got, want := f.Probes[0].Method, "GET"; got != want {
		t.Errorf("method = %q, want %q", got, want)
	}
	if !*f.Probes[0].FollowRedirects {
		t.Error("follow_redirects should default to true")
	}
	if got, want := f.Prober.MetricsListen, ":8081"; got != want {
		t.Errorf("metrics_listen = %q, want %q", got, want)
	}
	if got, want := f.Viewer.QueueSize, 10000; got != want {
		t.Errorf("queue_size = %d, want %d", got, want)
	}
	if got, want := f.Viewer.TokenEnv, "PULSE_INGEST_TOKEN"; got != want {
		t.Errorf("token_env = %q, want %q", got, want)
	}
}

func TestParsePerProbeOverrides(t *testing.T) {
	f, err := Parse([]byte(`
prober:
  name: p
defaults:
  interval: 60s
  timeout: 4s
probes:
  - name: fast
    type: tcp
    address: 192.0.2.10:5432
    interval: 15s
  - name: slow
    type: tcp
    address: 192.0.2.11:5432
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := f.Probes[0].Interval.D(), 15*time.Second; got != want {
		t.Errorf("fast interval = %s, want %s", got, want)
	}
	if got, want := f.Probes[1].Interval.D(), 60*time.Second; got != want {
		t.Errorf("slow interval = %s (should inherit defaults), want %s", got, want)
	}
	if got, want := f.Probes[0].Timeout.D(), 4*time.Second; got != want {
		t.Errorf("timeout = %s, want %s", got, want)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "unknown key is rejected",
			yaml: `
prober:
  name: p
probes:
  - name: api
    type: http
    url: https://example.com/
    intervall: 30s
`,
			want: "field intervall not found",
		},
		{
			name: "missing prober name",
			yaml: `
probes:
  - name: api
    type: http
    url: https://example.com/
`,
			want: "prober.name is required",
		},
		{
			name: "no probes",
			yaml: `
prober:
  name: p
`,
			want: "at least one target is required",
		},
		{
			name: "duplicate names",
			yaml: `
prober:
  name: p
probes:
  - name: api
    type: http
    url: https://example.com/
  - name: api
    type: http
    url: https://example.com/other
`,
			want: "duplicate target name",
		},
		{
			name: "http without url",
			yaml: `
prober:
  name: p
probes:
  - name: api
    type: http
`,
			want: "must be an absolute http(s) URL",
		},
		{
			name: "http with ftp url",
			yaml: `
prober:
  name: p
probes:
  - name: api
    type: http
    url: ftp://example.com/
`,
			want: "must be an absolute http(s) URL",
		},
		{
			name: "tcp without port",
			yaml: `
prober:
  name: p
probes:
  - name: db
    type: tcp
    address: 192.0.2.10
`,
			want: "must be host:port",
		},
		{
			name: "bad duration",
			yaml: `
prober:
  name: p
probes:
  - name: api
    type: http
    url: https://example.com/
    interval: often
`,
			want: "invalid duration",
		},
		{
			name: "timeout exceeding interval",
			yaml: `
prober:
  name: p
probes:
  - name: api
    type: http
    url: https://example.com/
    interval: 5s
    timeout: 10s
`,
			want: "must not exceed interval",
		},
		{
			name: "bad body regex",
			yaml: `
prober:
  name: p
probes:
  - name: api
    type: http
    url: https://example.com/
    expect:
      body_regex: "([a-z"
`,
			want: "body_regex",
		},
		{
			name: "json path without operator",
			yaml: `
prober:
  name: p
probes:
  - name: api
    type: http
    url: https://example.com/
    expect:
      json_path:
        path: "$.status"
`,
			want: "needs equals or contains",
		},
		{
			name: "invalid type",
			yaml: `
prober:
  name: p
probes:
  - name: api
    type: smtp
    address: mail.example.com:25
`,
			want: `type "smtp" must be http, tcp or icmp`,
		},
		{
			name: "maintenance window backwards",
			yaml: `
prober:
  name: p
probes:
  - name: api
    type: http
    url: https://example.com/
    maintenance:
      - from: "2026-11-21T02:00:00Z"
        to: "2026-11-20T22:00:00Z"
`,
			want: "ends before it starts",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	// One run of --check must list every problem, not just the first.
	_, err := Parse([]byte(`
prober:
  name: p
probes:
  - name: a
    type: http
  - name: b
    type: tcp
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"must be an absolute http(s) URL", "address is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestInMaintenance(t *testing.T) {
	from := time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)
	to := from.Add(4 * time.Hour)
	p := Probe{Maintenance: []Window{{From: from, To: to}}}

	if !p.InMaintenance(from.Add(time.Hour)) {
		t.Error("inside the window should be true")
	}
	if p.InMaintenance(from.Add(-time.Second)) {
		t.Error("before the window should be false")
	}
	if p.InMaintenance(to) {
		t.Error("the end is exclusive, so it should be false")
	}
}
