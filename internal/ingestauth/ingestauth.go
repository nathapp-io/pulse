// Package ingestauth decides whether a prober may push, and which prober it is
// allowed to claim to be.
//
// Tokens can be bound to a prober name, so a token leaked from one prober cannot
// be used to fabricate history for another. They can also come from a file that
// is re-read every TTL, because an env-only token can never be revoked without
// restarting the viewer -- and "revoked within one push interval" was the actual
// requirement.
package ingestauth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// AnyProber is the key used for a token shared by every prober.
const AnyProber = "*"

// DefaultTTL is how long a tokens file is trusted before it is read again.
const DefaultTTL = 2 * time.Second

// Options configures a Store.
type Options struct {
	// Static maps prober name (or AnyProber) to token.
	Static map[string]string
	// File, when set, is a "prober=token" file re-read every TTL.
	File string
	// TTL bounds the staleness of File. Defaults to DefaultTTL.
	TTL time.Duration
	// Now and OnError are injectable for tests and logging.
	Now     func() time.Time
	OnError func(error)
}

// Store authorises (prober, token) pairs.
type Store struct {
	static  map[string]string
	file    string
	ttl     time.Duration
	now     func() time.Time
	onError func(error)

	mu       sync.Mutex
	fromFile map[string]string
	loadedAt time.Time
	loaded   bool
}

// New validates the configuration and loads the file, if any, so a broken tokens
// file fails at startup rather than locking every prober out later.
func New(opts Options) (*Store, error) {
	s := &Store{
		static:  make(map[string]string, len(opts.Static)),
		file:    opts.File,
		ttl:     opts.TTL,
		now:     opts.Now,
		onError: opts.OnError,
	}
	if s.ttl <= 0 {
		s.ttl = DefaultTTL
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.onError == nil {
		s.onError = func(error) {}
	}

	for name, token := range opts.Static {
		if strings.TrimSpace(token) == "" {
			return nil, fmt.Errorf("ingest token for %q is empty", name)
		}
		s.static[name] = token
	}

	if s.file != "" {
		if err := s.reloadLocked(); err != nil {
			return nil, err
		}
	}

	if len(s.static) == 0 && len(s.fromFile) == 0 {
		return nil, errors.New("no ingest tokens configured: an open ingest endpoint accepts fabricated history")
	}
	return s, nil
}

// Authorize reports whether token is valid for prober. Every configured token is
// compared, in constant time, without early exit, so neither the response time
// nor the status code reveals which prober names exist.
func (s *Store) Authorize(prober, token string) bool {
	if token == "" {
		return false
	}
	tokens := s.current()
	authorized := false
	for name, want := range tokens {
		match := subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
		if match && (name == AnyProber || name == prober) {
			authorized = true
		}
	}
	return authorized
}

// ProberNames lists the prober names currently bound to a token, sorted.
func (s *Store) ProberNames() []string {
	tokens := s.current()
	names := make([]string, 0, len(tokens))
	for name := range tokens {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// current returns the effective token set, reloading the file when its TTL has
// expired. A file that has become unreadable keeps the last good set: a typo in
// a config file must not lock every prober out.
func (s *Store) current() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file != "" && (!s.loaded || s.now().Sub(s.loadedAt) >= s.ttl) {
		if err := s.reloadLocked(); err != nil {
			s.onError(err)
		}
	}

	out := make(map[string]string, len(s.static)+len(s.fromFile))
	for name, token := range s.static {
		out[name] = token
	}
	for name, token := range s.fromFile {
		out[name] = token
	}
	return out
}

// reloadLocked replaces fromFile from disk. The caller holds s.mu.
func (s *Store) reloadLocked() error {
	data, err := os.ReadFile(s.file)
	if err != nil {
		return fmt.Errorf("read ingest tokens file: %w", err)
	}
	parsed, err := parseTokens(data)
	if err != nil {
		return fmt.Errorf("parse %s: %w", s.file, err)
	}
	s.fromFile = parsed
	s.loadedAt = s.now()
	s.loaded = true
	return nil
}

// parseTokens reads "prober=token" lines, ignoring blank lines and comments.
func parseTokens(data []byte) (map[string]string, error) {
	out := make(map[string]string)
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, token, found := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		token = strings.TrimSpace(token)
		if !found || name == "" || token == "" {
			return nil, fmt.Errorf("line %d: expected prober=token", i+1)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("line %d: duplicate prober %q", i+1, name)
		}
		out[name] = token
	}
	return out, nil
}
