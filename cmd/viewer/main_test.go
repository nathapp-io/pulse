package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Inspect the resolved configuration without starting the API or handling a push.
func TestIngestAuthResolvesOneSource(t *testing.T) {
	file := filepath.Join(t.TempDir(), "bindings")
	if err := os.WriteFile(file, []byte("core=fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, file, bound, shared string
		want                      []string // nil means invalid configuration
	}{
		{"file", file, "stale value", "unused", []string{"core"}},
		{"missing file", file + ".missing", "edge=unused", "unused", nil},
		{"bound", "", "edge=fixture", "unused", []string{"edge"}},
		{"malformed bound", "", "missing binding", "unused", nil},
		{"shared", "", "", "fixture", []string{"*"}},
		{"none", "", "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PULSE_INGEST_TOKENS_FILE", tc.file)
			t.Setenv("PULSE_INGEST_TOKENS", tc.bound)
			t.Setenv("PULSE_INGEST_TOKEN", tc.shared)
			store, err := ingestAuth(slog.Default())
			if tc.want == nil {
				if err == nil {
					t.Fatal("invalid configuration accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := store.ProberNames(); !slices.Equal(got, tc.want) {
				t.Fatalf("probers = %v, want %v", got, tc.want)
			}
		})
	}
}
