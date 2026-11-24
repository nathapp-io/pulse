// Package httpapi exposes the viewer over HTTP: ingest for probers, status and
// history for the UI, and a health endpoint for orchestrators.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"pulse/internal/model"
	"pulse/internal/viewer"
)

// Config is the API's authentication and limit configuration.
type Config struct {
	// IngestToken is the bearer token probers must present. Required.
	IngestToken string
	// AuthUser and one of AuthPassword / AuthPasswordHash gate the read API.
	AuthUser         string
	AuthPassword     string
	AuthPasswordHash string
	// MaxBodyBytes caps an ingest request body.
	MaxBodyBytes int64
	// MaxHistoryWindow caps how far back a history request may reach.
	MaxHistoryWindow time.Duration
}

// Validate fails fast on a configuration that would be unsafe to serve.
func (c Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.IngestToken) == "" {
		errs = append(errs, errors.New("PULSE_INGEST_TOKEN is required: an open ingest endpoint accepts fabricated history"))
	}
	if c.AuthUser != "" && c.AuthPassword == "" && c.AuthPasswordHash == "" {
		errs = append(errs, errors.New("PULSE_AUTH_USER is set but neither PULSE_AUTH_PASSWORD nor PULSE_AUTH_PASSWORD_HASH is"))
	}
	if c.AuthPassword != "" && c.AuthPasswordHash != "" {
		errs = append(errs, errors.New("set only one of PULSE_AUTH_PASSWORD and PULSE_AUTH_PASSWORD_HASH"))
	}
	return errors.Join(errs...)
}

type handler struct {
	v      *viewer.Viewer
	cfg    Config
	logger *slog.Logger
}

// New builds the HTTP handler.
func New(v *viewer.Viewer, cfg Config, logger *slog.Logger) http.Handler {
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 4 << 20
	}
	if cfg.MaxHistoryWindow <= 0 {
		cfg.MaxHistoryWindow = 7 * 24 * time.Hour
	}
	if logger == nil {
		logger = slog.Default()
	}
	h := &handler{v: v, cfg: cfg, logger: logger}

	mux := http.NewServeMux()
	// Method checks are manual on purpose: ServeMux method patterns need Go 1.22
	// and this module supports 1.21.
	mux.HandleFunc("/api/v1/ingest", h.ingest)
	mux.HandleFunc("/api/v1/status", h.read(h.status))
	mux.HandleFunc("/api/v1/history", h.read(h.history))
	mux.HandleFunc("/healthz", h.healthz)
	return mux
}

// read wraps a read endpoint with Basic auth (when configured) and a GET check.
func (h *handler) read(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !h.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="pulse"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (h *handler) authorized(r *http.Request) bool {
	if h.cfg.AuthUser == "" {
		return true // auth disabled: startup already warned about it
	}
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(user), []byte(h.cfg.AuthUser)) != 1 {
		return false
	}
	if h.cfg.AuthPasswordHash != "" {
		return bcrypt.CompareHashAndPassword([]byte(h.cfg.AuthPasswordHash), []byte(pass)) == nil
	}
	return subtle.ConstantTimeCompare([]byte(pass), []byte(h.cfg.AuthPassword)) == 1
}

func (h *handler) ingest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(token), []byte(h.cfg.IngestToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	// Unknown fields are an error: a prober sending a field this viewer ignores
	// would otherwise lose data silently.
	dec.DisallowUnknownFields()

	var batch model.Batch
	if err := dec.Decode(&batch); err != nil {
		h.logger.Warn("ingest rejected", "error", err)
		http.Error(w, "invalid batch: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := batch.Validate(); err != nil {
		h.logger.Warn("ingest rejected", "prober", batch.Prober.Name, "error", err)
		http.Error(w, "invalid batch: "+err.Error(), http.StatusBadRequest)
		return
	}

	stored, err := h.v.Ingest(r.Context(), batch)
	if err != nil {
		h.logger.Error("ingest failed", "prober", batch.Prober.Name, "error", err)
		http.Error(w, "ingest failed", http.StatusInternalServerError)
		return
	}
	h.logger.Debug("ingest ok", "prober", batch.Prober.Name, "results", len(batch.Results), "new", stored)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) status(w http.ResponseWriter, r *http.Request) {
	st, err := h.v.Status(r.Context())
	if err != nil {
		h.logger.Error("status failed", "error", err)
		http.Error(w, "status unavailable", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *handler) history(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if strings.TrimSpace(target) == "" {
		http.Error(w, "target is required", http.StatusBadRequest)
		return
	}

	window := time.Hour
	if raw := r.URL.Query().Get("window"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			http.Error(w, "window must be a duration such as 1h or 24h", http.StatusBadRequest)
			return
		}
		window = parsed
	}
	if window > h.cfg.MaxHistoryWindow {
		window = h.cfg.MaxHistoryWindow
	}

	results, err := h.v.History(r.Context(), target, window)
	if err != nil {
		h.logger.Error("history failed", "target", target, "error", err)
		http.Error(w, "history unavailable", http.StatusInternalServerError)
		return
	}
	if results == nil {
		results = []model.Result{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"target":  target,
		"window":  window.String(),
		"results": results,
	})
}

func (h *handler) healthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}
