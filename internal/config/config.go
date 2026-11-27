// Package config loads and validates the prober YAML configuration.
//
// Validation is strict: unknown keys are a hard error, because a typo in a probe
// definition (say "intervall") would otherwise silently probe at the default rate
// forever. A rejected file never replaces a running configuration.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from a Go duration string ("30s", "5m").
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string such as \"30s\" (line %d)", node.Line)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q (line %d)", s, node.Line)
	}
	*d = Duration(v)
	return nil
}

// File is the whole prober configuration.
type File struct {
	SourceHash string   `yaml:"-"` // Hash of the validated source bytes.
	Prober     Prober   `yaml:"prober"`
	Viewer     Viewer   `yaml:"viewer"`
	Defaults   Defaults `yaml:"defaults"`
	Probes     []Probe  `yaml:"probes"`
}

// Prober describes the prober process itself.
type Prober struct {
	Name                string            `yaml:"name"`
	Labels              map[string]string `yaml:"labels"`
	MetricsListen       string            `yaml:"metrics_listen"`
	MaxConcurrentProbes int               `yaml:"max_concurrent_probes"`
}

// Viewer describes where results are pushed. The viewer never connects back.
type Viewer struct {
	URL                   string   `yaml:"url"`
	PushInterval          Duration `yaml:"push_interval"`
	PushTimeout           Duration `yaml:"push_timeout"`
	QueueSize             int      `yaml:"queue_size"`
	TokenEnv              string   `yaml:"token_env"`
	TLSInsecureSkipVerify bool     `yaml:"tls_insecure_skip_verify"`
}

// Defaults are inherited by every probe that does not set the key itself.
type Defaults struct {
	Interval        Duration `yaml:"interval"`
	Timeout         Duration `yaml:"timeout"`
	ConfirmDown     int      `yaml:"confirm_down"`
	ConfirmUp       int      `yaml:"confirm_up"`
	FollowRedirects *bool    `yaml:"follow_redirects"`
	MaxBodyBytes    int64    `yaml:"max_body_bytes"`
}

// Probe is one monitored target.
type Probe struct {
	Name            string            `yaml:"name"`
	Group           string            `yaml:"group"`
	Type            string            `yaml:"type"`
	URL             string            `yaml:"url"`
	Address         string            `yaml:"address"`
	Method          string            `yaml:"method"`
	Headers         map[string]string `yaml:"headers"`
	Interval        Duration          `yaml:"interval"`
	Timeout         Duration          `yaml:"timeout"`
	ConfirmDown     int               `yaml:"confirm_down"`
	ConfirmUp       int               `yaml:"confirm_up"`
	FollowRedirects *bool             `yaml:"follow_redirects"`
	MaxBodyBytes    int64             `yaml:"max_body_bytes"`
	Expect          Expect            `yaml:"expect"`
	Maintenance     []Window          `yaml:"maintenance"`
}

// Expect holds the assertions applied to a successful HTTP response.
type Expect struct {
	Status       []int             `yaml:"status"`
	BodyRegex    string            `yaml:"body_regex"`
	JSONPath     *JSONPathCheck    `yaml:"json_path"`
	Header       map[string]string `yaml:"header"`
	TLSExpiryMin Duration          `yaml:"tls_expiry_min"`
}

// JSONPathCheck asserts a value from a dotted path subset such as "$.db.status".
type JSONPathCheck struct {
	Path     string `yaml:"path"`
	Equals   string `yaml:"equals"`
	Contains string `yaml:"contains"`
}

// Window is a maintenance period: results are still recorded, but the target is
// not allowed to flap the UI or (later) alerting during it.
type Window struct {
	From   time.Time `yaml:"from"`
	To     time.Time `yaml:"to"`
	Reason string    `yaml:"reason"`
}

// Load reads, defaults and validates a configuration file.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	f, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// Parse decodes and validates configuration bytes.
func Parse(b []byte) (*File, error) {
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true) // unknown keys are a hard error, not a silent no-op
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	f.applyDefaults()
	if err := f.Validate(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	f.SourceHash = hex.EncodeToString(sum[:8])
	return &f, nil
}

func boolPtr(b bool) *bool { return &b }

func (f *File) applyDefaults() {
	if f.Prober.MetricsListen == "" {
		f.Prober.MetricsListen = ":8081"
	}
	if f.Prober.MaxConcurrentProbes <= 0 {
		f.Prober.MaxConcurrentProbes = 32
	}
	if f.Defaults.Interval == 0 {
		f.Defaults.Interval = Duration(30 * time.Second)
	}
	if f.Defaults.Timeout == 0 {
		f.Defaults.Timeout = Duration(5 * time.Second)
	}
	if f.Defaults.ConfirmDown <= 0 {
		f.Defaults.ConfirmDown = 2
	}
	if f.Defaults.ConfirmUp <= 0 {
		f.Defaults.ConfirmUp = 1
	}
	if f.Defaults.FollowRedirects == nil {
		f.Defaults.FollowRedirects = boolPtr(true)
	}
	if f.Defaults.MaxBodyBytes <= 0 {
		f.Defaults.MaxBodyBytes = 64 << 10
	}

	if f.Viewer.PushInterval == 0 {
		f.Viewer.PushInterval = Duration(10 * time.Second)
	}
	if f.Viewer.PushTimeout == 0 {
		f.Viewer.PushTimeout = Duration(5 * time.Second)
	}
	if f.Viewer.QueueSize <= 0 {
		f.Viewer.QueueSize = 10000
	}
	if f.Viewer.TokenEnv == "" {
		f.Viewer.TokenEnv = "PULSE_INGEST_TOKEN"
	}

	for i := range f.Probes {
		p := &f.Probes[i]
		if p.Group == "" {
			p.Group = "default"
		}
		if p.Method == "" {
			p.Method = http.MethodGet
		}
		if p.Interval == 0 {
			p.Interval = f.Defaults.Interval
		}
		if p.Timeout == 0 {
			p.Timeout = f.Defaults.Timeout
		}
		if p.ConfirmDown <= 0 {
			p.ConfirmDown = f.Defaults.ConfirmDown
		}
		if p.ConfirmUp <= 0 {
			p.ConfirmUp = f.Defaults.ConfirmUp
		}
		if p.FollowRedirects == nil {
			p.FollowRedirects = f.Defaults.FollowRedirects
		}
		if p.MaxBodyBytes <= 0 {
			p.MaxBodyBytes = f.Defaults.MaxBodyBytes
		}
	}
}

// Validate reports every problem at once, so one run of --check fixes them all.
func (f *File) Validate() error {
	var errs []error

	if strings.TrimSpace(f.Prober.Name) == "" {
		errs = append(errs, errors.New("prober.name is required (the viewer attributes every result to it)"))
	}
	if len(f.Probes) == 0 {
		errs = append(errs, errors.New("probes: at least one target is required"))
	}
	if f.Viewer.URL != "" {
		u, err := url.Parse(f.Viewer.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("viewer.url %q must be an absolute http(s) URL", f.Viewer.URL))
		}
	}

	seen := make(map[string]bool, len(f.Probes))
	for i := range f.Probes {
		p := &f.Probes[i]
		where := fmt.Sprintf("probes[%d]", i)
		if p.Name != "" {
			where = fmt.Sprintf("probes[%d] (%s)", i, p.Name)
		}

		switch {
		case p.Name == "":
			errs = append(errs, fmt.Errorf("%s: name is required", where))
		case seen[p.Name]:
			errs = append(errs, fmt.Errorf("%s: duplicate target name", where))
		default:
			seen[p.Name] = true
		}

		if p.Interval.D() <= 0 {
			errs = append(errs, fmt.Errorf("%s: interval must be positive", where))
		}
		if p.Timeout.D() <= 0 {
			errs = append(errs, fmt.Errorf("%s: timeout must be positive", where))
		} else if p.Timeout.D() > p.Interval.D() {
			errs = append(errs, fmt.Errorf("%s: timeout (%s) must not exceed interval (%s)", where, p.Timeout, p.Interval))
		}
		if p.ConfirmDown <= 0 || p.ConfirmUp <= 0 {
			errs = append(errs, fmt.Errorf("%s: confirm_down and confirm_up must be positive", where))
		}

		switch p.Type {
		case "http":
			errs = append(errs, validateHTTP(p, where)...)
		case "tcp":
			if p.Address == "" {
				errs = append(errs, fmt.Errorf("%s: address is required for type tcp", where))
			} else if !strings.Contains(p.Address, ":") {
				errs = append(errs, fmt.Errorf("%s: address %q must be host:port", where, p.Address))
			}
		case "icmp":
			if p.Address == "" {
				errs = append(errs, fmt.Errorf("%s: address is required for type icmp", where))
			}
		case "":
			errs = append(errs, fmt.Errorf("%s: type is required (http, tcp or icmp)", where))
		default:
			errs = append(errs, fmt.Errorf("%s: type %q must be http, tcp or icmp", where, p.Type))
		}

		for _, w := range p.Maintenance {
			if w.From.IsZero() || w.To.IsZero() {
				errs = append(errs, fmt.Errorf("%s: maintenance windows need both from and to", where))
				continue
			}
			if !w.To.After(w.From) {
				errs = append(errs, fmt.Errorf("%s: maintenance window ends before it starts", where))
			}
		}
	}

	return errors.Join(errs...)
}

func validateHTTP(p *Probe, where string) []error {
	var errs []error

	u, err := url.Parse(p.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("%s: url %q must be an absolute http(s) URL", where, p.URL))
	}
	for _, s := range p.Expect.Status {
		if s < 100 || s > 599 {
			errs = append(errs, fmt.Errorf("%s: expect.status %d is not a valid status code", where, s))
		}
	}
	if p.Expect.BodyRegex != "" {
		if _, err := regexp.Compile(p.Expect.BodyRegex); err != nil {
			errs = append(errs, fmt.Errorf("%s: expect.body_regex: %w", where, err))
		}
	}
	for name, expr := range p.Expect.Header {
		if _, err := regexp.Compile(expr); err != nil {
			errs = append(errs, fmt.Errorf("%s: expect.header[%s]: %w", where, name, err))
		}
	}
	if jp := p.Expect.JSONPath; jp != nil {
		if !strings.HasPrefix(jp.Path, "$.") {
			errs = append(errs, fmt.Errorf("%s: expect.json_path.path %q must start with \"$.\"", where, jp.Path))
		}
		if jp.Equals == "" && jp.Contains == "" {
			errs = append(errs, fmt.Errorf("%s: expect.json_path needs equals or contains", where))
		}
	}
	return errs
}

// InMaintenance reports whether at falls inside one of the probe's windows.
func (p *Probe) InMaintenance(at time.Time) bool {
	for _, w := range p.Maintenance {
		if !at.Before(w.From) && at.Before(w.To) {
			return true
		}
	}
	return false
}
