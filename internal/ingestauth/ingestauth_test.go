package ingestauth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustNew(t *testing.T, opts Options) *Store {
	t.Helper()
	store, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store
}

func TestTokensAreBoundToTheirProber(t *testing.T) {
	s := mustNew(t, Options{Static: map[string]string{
		"prober-a": "token-a",
		"prober-b": "token-b",
	}})

	cases := []struct {
		prober, token string
		want          bool
	}{
		{"prober-a", "token-a", true},
		{"prober-b", "token-b", true},
		// The whole point of binding: a leaked token cannot write another
		// prober's history.
		{"prober-b", "token-a", false},
		{"prober-a", "token-b", false},
		{"prober-c", "token-a", false},
		{"prober-a", "", false},
		{"prober-a", "token-a ", false},
		{"", "token-a", false},
	}
	for _, tc := range cases {
		if got := s.Authorize(tc.prober, tc.token); got != tc.want {
			t.Fatalf("Authorize(%q, %q) = %v, want %v", tc.prober, tc.token, got, tc.want)
		}
	}
}

func TestSharedTokenAcceptsAnyProber(t *testing.T) {
	s := mustNew(t, Options{Static: map[string]string{AnyProber: "shared"}})
	for _, prober := range []string{"prober-a", "prober-b", "anything"} {
		if !s.Authorize(prober, "shared") {
			t.Fatalf("%q rejected a shared token", prober)
		}
	}
	if s.Authorize("prober-a", "other") {
		t.Fatal("shared token accepted a wrong value")
	}
}

func TestFileTokenRevocationTakesEffectWithoutRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write tokens: %v", err)
		}
	}
	write("prober-a=live-token\n")

	now := time.Unix(1_000_000, 0)
	clock := func() time.Time { return now }
	advance := func() { now = now.Add(2 * DefaultTTL) }

	s := mustNew(t, Options{File: path, Now: clock})
	if !s.Authorize("prober-a", "live-token") {
		t.Fatal("token from the file was rejected while it was valid")
	}

	// Revoking is just editing the file; the viewer must not need a restart.
	write("# prober-a rotated\n")
	advance()
	if s.Authorize("prober-a", "live-token") {
		t.Fatal("revoked token was still accepted after the TTL expired")
	}

	write("prober-a=rotated-token\n")
	advance()
	if !s.Authorize("prober-a", "rotated-token") {
		t.Fatal("rotated token was not accepted")
	}
}

func TestStaleFileKeepsLastGoodTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens")
	if err := os.WriteFile(path, []byte("prober-a=tok\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	now := time.Unix(1_000_000, 0)
	var reported []error
	s := mustNew(t, Options{
		File:    path,
		Now:     func() time.Time { return now },
		OnError: func(err error) { reported = append(reported, err) },
	})
	if !s.Authorize("prober-a", "tok") {
		t.Fatal("initial token rejected")
	}

	// A broken file must not lock every prober out: keep the last good set.
	if err := os.WriteFile(path, []byte("garbage-without-an-equals-sign\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	now = now.Add(2 * DefaultTTL)
	if !s.Authorize("prober-a", "tok") {
		t.Fatal("a malformed file dropped the last good tokens instead of keeping them")
	}
	if len(reported) == 0 {
		t.Fatal("a malformed file was not reported to OnError")
	}
}

func TestUnreadableFileKeepsLastGoodTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens")
	if err := os.WriteFile(path, []byte("prober-a=tok\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	now := time.Unix(1_000_000, 0)
	s := mustNew(t, Options{File: path, Now: func() time.Time { return now }})
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	now = now.Add(2 * DefaultTTL)
	if !s.Authorize("prober-a", "tok") {
		t.Fatal("a missing tokens file dropped the last good tokens")
	}
}

func TestNewRejectsBrokenConfiguration(t *testing.T) {
	dir := t.TempDir()

	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("prober-a\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	dup := filepath.Join(dir, "dup")
	if err := os.WriteFile(dup, []byte("a=1\na=2\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cases := map[string]Options{
		"no tokens at all":  {},
		"empty token value": {Static: map[string]string{"prober-a": "  "}},
		"missing file":      {File: filepath.Join(dir, "nope")},
		"malformed line":    {File: bad},
		"duplicate prober":  {File: dup},
	}
	for name, opts := range cases {
		if _, err := New(opts); err == nil {
			t.Fatalf("%s: New accepted an invalid configuration", name)
		}
	}
}

func TestFileIsReReadOnlyAfterTTL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens")
	if err := os.WriteFile(path, []byte("prober-a=one\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	now := time.Unix(1_000_000, 0)
	s := mustNew(t, Options{File: path, TTL: 5 * time.Second, Now: func() time.Time { return now }})

	if err := os.WriteFile(path, []byte("prober-a=two\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	// Within the TTL the old token is still trusted: that is the documented
	// staleness bound, and it is why revocation is "within one TTL".
	if s.Authorize("prober-a", "two") {
		t.Fatal("file was re-read before its TTL expired")
	}
	now = now.Add(6 * time.Second)
	if !s.Authorize("prober-a", "two") {
		t.Fatal("file was not re-read after its TTL expired")
	}
}

func TestParseTokensReportsLineNumbers(t *testing.T) {
	_, err := parseTokens([]byte("# ok\nprober-a=tok\nbroken-line\n"))
	if err == nil {
		t.Fatal("parseTokens accepted a malformed line")
	}
	var want string = "line 3"
	if !errors.Is(err, err) || !contains(err.Error(), want) {
		t.Fatalf("error %q does not name the offending line", err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
