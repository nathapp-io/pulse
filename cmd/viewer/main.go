// Command viewer ingests pushed probe results and serves the UI.
//
// Stub as of M1: the ingest endpoint, SQLite history, staleness accounting and
// the embedded UI arrive in M2/M3. It exists now so `go build ./...` covers the
// whole repository surface from the start.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "pulse-viewer is not implemented yet (M2/M3); run ./cmd/prober for now")
	os.Exit(1)
}
