// Command viewer receives probe results from probers and serves them over HTTP.
//
// Storage is pluggable: PULSE_STORE=memory keeps bounded rings in RAM (zero setup,
// history lost on restart) and PULSE_STORE=sqlite persists to a single file.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"pulse/internal/build"
	"pulse/internal/httpapi"
	"pulse/internal/ingestauth"
	"pulse/internal/store"
	"pulse/internal/viewer"
)

func main() {
	listen := flag.String("listen", envOr("PULSE_LISTEN", ":8080"), "address to serve the API on")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	storeCfg, err := storeConfig()
	if err != nil {
		logger.Error("invalid storage configuration", "error", err)
		os.Exit(1)
	}
	st, err := store.Open(storeCfg)
	if err != nil {
		logger.Error("cannot open store", "error", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	v := viewer.New(st, viewer.Config{
		StaleAfter:      durationOr("PULSE_STALE_AFTER", 30*time.Second),
		ExpectedProbers: csvOr("PULSE_EXPECTED_PROBERS", nil),
	})

	ingest, err := ingestAuth(logger)
	if err != nil {
		logger.Error("cannot configure ingest authentication", "error", err)
		os.Exit(1)
	}

	apiCfg := httpapi.Config{
		Ingest:           ingest,
		AuthUser:         os.Getenv("PULSE_AUTH_USER"),
		AuthPassword:     os.Getenv("PULSE_AUTH_PASSWORD"),
		AuthPasswordHash: os.Getenv("PULSE_AUTH_PASSWORD_HASH"),
	}
	if err := apiCfg.Validate(); err != nil {
		logger.Error("invalid API configuration", "error", err)
		os.Exit(1)
	}
	if apiCfg.AuthUser == "" {
		logger.Warn("PULSE_AUTH_USER is unset: the read API is unauthenticated; terminate TLS and restrict access in front of it")
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           httpapi.New(v, apiCfg, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("viewer started",
			"version", build.Version,
			"listen", *listen,
			"store", storeCfg.Kind,
			"db", storeCfg.Path,
			"stale_after", v.StaleAfter(),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// storeConfig resolves the backend from the environment.
//
// PULSE_STORE wins when set; otherwise a PULSE_DB path implies sqlite and its
// absence implies memory, so `docker run` works with no configuration at all.
func storeConfig() (store.Config, error) {
	kind := strings.ToLower(strings.TrimSpace(os.Getenv("PULSE_STORE")))
	path := strings.TrimSpace(os.Getenv("PULSE_DB"))
	if kind == "" {
		if path != "" {
			kind = "sqlite"
		} else {
			kind = "memory"
		}
	}

	cfg := store.Config{
		Kind:         kind,
		Path:         path,
		Retention:    durationOr("PULSE_RETENTION_RAW", 7*24*time.Hour),
		Retention1M:  durationOr("PULSE_RETENTION_1M", 30*24*time.Hour),
		Retention1H:  durationOr("PULSE_RETENTION_1H", 365*24*time.Hour),
		MaxPerTarget: intOr("PULSE_MEMORY_POINTS", 2880),
	}
	if kind == "sqlite" && path == "" {
		return cfg, errors.New("PULSE_STORE=sqlite requires PULSE_DB (path to the database file)")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func durationOr(key string, fallback time.Duration) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

func intOr(key string, fallback int) int {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func csvOr(key string, fallback []string) []string {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ingestAuth builds the prober authentication from the environment.
//
//	PULSE_INGEST_TOKEN       one token, valid for any prober name
//	PULSE_INGEST_TOKENS      "prober-a=tok1,prober-b=tok2" -- bound to a prober
//	PULSE_INGEST_TOKENS_FILE a file of "prober=token" lines, re-read every 2s so a
//	                         revoked token stops working without a restart
func ingestAuth(logger *slog.Logger) (*ingestauth.Store, error) {
	static := make(map[string]string, 2)
	if raw := strings.TrimSpace(os.Getenv("PULSE_INGEST_TOKENS")); raw != "" {
		for _, pair := range strings.Split(raw, ",") {
			name, token, found := strings.Cut(strings.TrimSpace(pair), "=")
			if !found || strings.TrimSpace(name) == "" || strings.TrimSpace(token) == "" {
				return nil, fmt.Errorf("PULSE_INGEST_TOKENS entry %q is not prober=token", strings.TrimSpace(pair))
			}
			static[strings.TrimSpace(name)] = strings.TrimSpace(token)
		}
	}
	if token := strings.TrimSpace(os.Getenv("PULSE_INGEST_TOKEN")); token != "" {
		static[ingestauth.AnyProber] = token
	}

	return ingestauth.New(ingestauth.Options{
		Static: static,
		File:   strings.TrimSpace(os.Getenv("PULSE_INGEST_TOKENS_FILE")),
		OnError: func(err error) {
			logger.Warn("ingest tokens file unusable, keeping the last good tokens", "error", err)
		},
	})
}
