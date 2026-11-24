package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"pulse/internal/ingestauth"
	"pulse/internal/model"
	"pulse/internal/store"
	"pulse/internal/viewer"
)

const (
	testToken = "ingest-secret"
	testUser  = "operator"
	testPass  = "hunter2"
)

func newTestHandler(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	if cfg.Ingest == nil {
		cfg.Ingest = newIngestAuth(t, map[string]string{ingestauth.AnyProber: testToken})
	}
	st, err := store.Open(store.Config{Kind: "memory", Retention: time.Hour, MaxPerTarget: 100})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(viewer.New(st, viewer.Config{StaleAfter: time.Minute}), cfg, nil)
}

func newIngestAuth(t *testing.T, static map[string]string) *ingestauth.Store {
	t.Helper()
	auth, err := ingestauth.New(ingestauth.Options{Static: static})
	if err != nil {
		t.Fatalf("ingest auth: %v", err)
	}
	return auth
}

func sampleBatch(at time.Time) model.Batch {
	return model.Batch{
		Prober:  model.ProberInfo{Name: "prober-a", Version: "test"},
		Targets: []model.TargetSpec{{Name: "api", Group: "web", Type: "http"}},
		Results: []model.Result{{Target: "api", At: at, OK: true, State: "UP", DurationMS: 9}},
	}
}

func postIngest(t *testing.T, h http.Handler, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestIngestRequiresToken(t *testing.T) {
	h := newTestHandler(t, Config{})
	body, _ := json.Marshal(sampleBatch(time.Now().UTC()))

	if rec := postIngest(t, h, "", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", rec.Code)
	}
	if rec := postIngest(t, h, "wrong", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", rec.Code)
	}
}

func TestIngestStoresAndIsIdempotent(t *testing.T) {
	h := newTestHandler(t, Config{})
	at := time.Now().UTC()
	body, _ := json.Marshal(sampleBatch(at))

	if rec := postIngest(t, h, testToken, body); rec.Code != http.StatusNoContent {
		t.Fatalf("first ingest: status = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	// A prober retrying the same batch must not be an error.
	if rec := postIngest(t, h, testToken, body); rec.Code != http.StatusNoContent {
		t.Fatalf("replay: status = %d, want 204", rec.Code)
	}

	rec := get(t, h, "/api/v1/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var st viewer.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(st.Targets) != 1 || st.Targets[0].State != "UP" {
		t.Fatalf("targets = %+v, want one UP target", st.Targets)
	}

	// History must hold exactly one sample, not two.
	hrec := get(t, h, "/api/v1/history?target=api&window=1h", nil)
	if hrec.Code != http.StatusOK {
		t.Fatalf("history status = %d, want 200", hrec.Code)
	}
	var payload struct {
		Results []model.Result `json:"results"`
	}
	if err := json.Unmarshal(hrec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(payload.Results) != 1 {
		t.Fatalf("history results = %d after replay, want 1", len(payload.Results))
	}
}

func TestIngestRejectsUnknownFields(t *testing.T) {
	h := newTestHandler(t, Config{})
	body := []byte(`{"prober":{"name":"p"},"results":[{"target":"api","at":"2026-01-01T00:00:00Z"}],"typo_field":true}`)
	if rec := postIngest(t, h, testToken, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown field", rec.Code)
	}
}

func TestIngestRejectsInvalidBatch(t *testing.T) {
	h := newTestHandler(t, Config{})
	body := []byte(`{"prober":{"name":"p"},"results":[]}`)
	if rec := postIngest(t, h, testToken, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func get(t *testing.T, h http.Handler, path string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func withBasic(user, pass string) func(*http.Request) {
	return func(r *http.Request) { r.SetBasicAuth(user, pass) }
}

func TestReadAPIRequiresBasicAuth(t *testing.T) {
	h := newTestHandler(t, Config{AuthUser: testUser, AuthPassword: testPass})

	rec := get(t, h, "/api/v1/status", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials: status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate header")
	}
	if rec := get(t, h, "/api/v1/status", withBasic(testUser, "wrong")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: status = %d, want 401", rec.Code)
	}
	if rec := get(t, h, "/api/v1/status", withBasic(testUser, testPass)); rec.Code != http.StatusOK {
		t.Fatalf("correct credentials: status = %d, want 200", rec.Code)
	}
}

func TestReadAPIAcceptsBcryptHash(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte(testPass), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	h := newTestHandler(t, Config{AuthUser: testUser, AuthPasswordHash: string(hash)})

	if rec := get(t, h, "/api/v1/status", withBasic(testUser, testPass)); rec.Code != http.StatusOK {
		t.Fatalf("bcrypt credentials: status = %d, want 200", rec.Code)
	}
	if rec := get(t, h, "/api/v1/status", withBasic(testUser, "nope")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad bcrypt credentials: status = %d, want 401", rec.Code)
	}
}

func TestHistoryValidatesParams(t *testing.T) {
	h := newTestHandler(t, Config{})
	if rec := get(t, h, "/api/v1/history", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing target: status = %d, want 400", rec.Code)
	}
	if rec := get(t, h, "/api/v1/history?target=api&window=nonsense", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad window: status = %d, want 400", rec.Code)
	}
}

func TestMethodsAreEnforced(t *testing.T) {
	h := newTestHandler(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ingest", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET ingest: status = %d, want 405", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/status", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status: status = %d, want 405", rec.Code)
	}
}

func TestHealthzIsOpen(t *testing.T) {
	h := newTestHandler(t, Config{AuthUser: testUser, AuthPassword: testPass})
	if rec := get(t, h, "/healthz", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200 without credentials", rec.Code)
	}
}

func TestConfigValidate(t *testing.T) {
	auth := newIngestAuth(t, map[string]string{ingestauth.AnyProber: "t"})
	cases := map[string]struct {
		cfg     Config
		wantErr bool
	}{
		"missing token":      {Config{}, true},
		"user without pass":  {Config{Ingest: auth, AuthUser: "u"}, true},
		"both pass and hash": {Config{Ingest: auth, AuthUser: "u", AuthPassword: "p", AuthPasswordHash: "h"}, true},
		"plain password":     {Config{Ingest: auth, AuthUser: "u", AuthPassword: "p"}, false},
		"no auth at all":     {Config{Ingest: auth}, false},
	}
	for name, tc := range cases {
		err := tc.cfg.Validate()
		if tc.wantErr && err == nil {
			t.Fatalf("%s: Validate accepted an unsafe config", name)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("%s: Validate rejected a valid config: %v", name, err)
		}
	}
}

func TestServesEmbeddedUI(t *testing.T) {
	h := newTestHandler(t, Config{AuthUser: testUser, AuthPassword: testPass})

	// The UI sits behind the same auth as the API.
	if rec := get(t, h, "/", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated index: status = %d, want 401", rec.Code)
	}

	rec := get(t, h, "/", withBasic(testUser, testPass))
	if rec.Code != http.StatusOK {
		t.Fatalf("index: status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("index content-type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "pulse") || !strings.Contains(body, "/static/app.js") {
		t.Fatalf("index body does not look like the UI: %q", body[:min(len(body), 120)])
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("index is served without a Content-Security-Policy")
	}

	for _, asset := range []string{
		"/static/app.js",
		"/static/styles.css",
		"/static/vendor/uplot.iife.min.js",
		"/static/vendor/uplot.min.css",
	} {
		if rec := get(t, h, asset, withBasic(testUser, testPass)); rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", asset, rec.Code)
		}
	}

	if rec := get(t, h, "/static/../go.mod", withBasic(testUser, testPass)); rec.Code == http.StatusOK {
		t.Fatal("path traversal outside the embedded assets was served")
	}
	if rec := get(t, h, "/nope", withBasic(testUser, testPass)); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: status = %d, want 404", rec.Code)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestIngestTokenIsBoundToItsProber(t *testing.T) {
	auth := newIngestAuth(t, map[string]string{
		"prober-a": "token-a",
		"prober-b": "token-b",
	})
	h := newTestHandler(t, Config{Ingest: auth})

	body, _ := json.Marshal(sampleBatch(time.Now().UTC())) // claims to be prober-a

	if rec := postIngest(t, h, "token-b", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("prober-b's token for prober-a's history: status = %d, want 401", rec.Code)
	}
	if rec := postIngest(t, h, "token-a", body); rec.Code != http.StatusNoContent {
		t.Fatalf("own token: status = %d, want 204", rec.Code)
	}
}

func TestRevokedIngestTokenIsRejectedWithoutRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte("prober-a=token-a\n"), 0o600); err != nil {
		t.Fatalf("write tokens: %v", err)
	}

	now := time.Unix(1_000_000, 0)
	auth, err := ingestauth.New(ingestauth.Options{
		File: path,
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("ingestauth.New: %v", err)
	}
	h := newTestHandler(t, Config{Ingest: auth})

	body, _ := json.Marshal(sampleBatch(time.Now().UTC()))
	if rec := postIngest(t, h, "token-a", body); rec.Code != http.StatusNoContent {
		t.Fatalf("valid token: status = %d, want 204", rec.Code)
	}

	// Rotate the prober out of the file; no restart, no new process.
	if err := os.WriteFile(path, []byte("prober-a=rotated\n"), 0o600); err != nil {
		t.Fatalf("rewrite tokens: %v", err)
	}
	now = now.Add(2 * ingestauth.DefaultTTL)

	if rec := postIngest(t, h, "token-a", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token: status = %d, want 401 within one TTL", rec.Code)
	}
	if rec := postIngest(t, h, "rotated", body); rec.Code != http.StatusNoContent {
		t.Fatalf("rotated token: status = %d, want 204", rec.Code)
	}
}
