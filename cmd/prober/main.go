// Command prober runs the pulse exporter: it probes every configured target,
// exposes the results as Prometheus metrics, and pushes them to the viewer when
// viewer.url is configured.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"pulse/internal/build"
	"pulse/internal/config"
	"pulse/internal/metrics"
	"pulse/internal/model"
	"pulse/internal/probe"
	"pulse/internal/push"
	"pulse/internal/schedule"
	"pulse/internal/state"
	"pulse/internal/uptime"
)

// targetRuntime is everything the prober keeps for one target.
type targetRuntime struct {
	cfg     config.Probe
	prober  probe.Prober
	machine *state.Machine
	buckets *uptime.Buckets
}

// windows are the rolling uptime windows reported to /metrics and, later, pushed.
var windows = []struct {
	name string
	d    time.Duration
}{
	{"1h", time.Hour},
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to the prober configuration file")
	check := flag.Bool("check", false, "validate the configuration and exit")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("configuration rejected", "path", *configPath, "error", err)
		os.Exit(1)
	}
	if *check {
		fmt.Printf("ok: prober %q, %d target(s), version %s\n", cfg.Prober.Name, len(cfg.Probes), build.Version)
		return
	}

	m := metrics.New()
	targets, err := buildTargets(cfg, nil)
	if err != nil {
		logger.Error("cannot build probes", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var (
		mu      sync.RWMutex
		current = targets
	)

	targetsSpec := func() []model.TargetSpec {
		mu.RLock()
		defer mu.RUnlock()
		out := make([]model.TargetSpec, 0, len(current))
		for name, rt := range current {
			out = append(out, model.TargetSpec{Name: name, Group: rt.cfg.Group, Type: rt.cfg.Type})
		}
		return out
	}

	var (
		pusher   *push.Client
		pushDone chan struct{}
	)
	if cfg.Viewer.URL != "" {
		token := strings.TrimSpace(os.Getenv(cfg.Viewer.TokenEnv))
		if token == "" {
			logger.Error("cannot push without a token: the configured environment variable is empty",
				"env", cfg.Viewer.TokenEnv)
			os.Exit(1)
		}
		pusher, err = push.New(push.Config{
			URL:                cfg.Viewer.URL,
			Token:              token,
			Interval:           cfg.Viewer.PushInterval.D(),
			Timeout:            cfg.Viewer.PushTimeout.D(),
			QueueSize:          cfg.Viewer.QueueSize,
			InsecureSkipVerify: cfg.Viewer.TLSInsecureSkipVerify,
			ProberName:         cfg.Prober.Name,
			ProberVersion:      build.Version,
			Labels:             cfg.Prober.Labels,
			ConfigHash:         configHash(*configPath),
			Targets:            targetsSpec,
		}, m)
		if err != nil {
			logger.Error("cannot start the push client", "error", err)
			os.Exit(1)
		}
		pushDone = make(chan struct{})
		go func() {
			defer close(pushDone)
			pusher.Run(ctx)
		}()
		logger.Info("pushing results to the viewer",
			"url", cfg.Viewer.URL,
			"interval", cfg.Viewer.PushInterval.D(),
			"queue_size", cfg.Viewer.QueueSize,
		)
	} else {
		logger.Warn("viewer.url is not set: results are only exposed on /metrics")
	}

	handle := func(res probe.Result) {
		mu.RLock()
		rt := current[res.Target]
		mu.RUnlock()
		if rt == nil {
			return
		}

		transition, changed := rt.machine.Observe(res.OK, res.At)
		rt.buckets.Record(res.OK, res.At)
		m.Observe(res)
		for _, w := range windows {
			m.SetUptime(res.Target, res.Group, w.name, rt.buckets.Ratio(w.d))
		}

		if pusher != nil {
			currentState, _ := rt.machine.Snapshot()
			pusher.Enqueue(toModelResult(res, currentState))
			if changed {
				// A state change must reach the viewer in seconds, not at the
				// next batch interval.
				pusher.FlushNow()
			}
		}

		if !res.OK {
			logger.Warn("probe failed",
				"target", res.Target,
				"class", res.ErrorClass,
				"status", res.StatusCode,
				"duration_ms", res.DurationMS,
				"error", res.Error,
			)
		}
		if changed {
			logger.Info("state change",
				"target", res.Target,
				"from", transition.From,
				"to", transition.To,
				"class", res.ErrorClass,
			)
		}
	}

	runner := schedule.New(cfg.Prober.MaxConcurrentProbes)
	runner.Start(ctx, toSchedule(targets), handle)

	server := &http.Server{
		Addr:              cfg.Prober.MetricsListen,
		Handler:           metricsMux(m),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("prober started",
			"version", build.Version,
			"name", cfg.Prober.Name,
			"targets", len(targets),
			"metrics_addr", cfg.Prober.MetricsListen,
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "error", err)
			stop()
		}
	}()

	go reloadOnHUP(*configPath, logger, runner, ctx, &mu, &current, handle)

	<-ctx.Done()
	logger.Info("shutting down")

	runner.Stop()

	// The run context is already cancelled: wait for the final flush so results
	// the viewer never saw are not thrown away on a clean shutdown.
	if pushDone != nil {
		<-pushDone
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

// reloadOnHUP applies a new configuration only if it is valid; a rejected file
// leaves the running configuration untouched.
func reloadOnHUP(
	path string,
	logger *slog.Logger,
	runner *schedule.Runner,
	ctx context.Context,
	mu *sync.RWMutex,
	current *map[string]*targetRuntime,
	handle schedule.Handler,
) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			newCfg, err := config.Load(path)
			if err != nil {
				logger.Error("reload rejected, keeping the running configuration", "error", err)
				continue
			}

			mu.RLock()
			previous := *current
			mu.RUnlock()

			newTargets, err := buildTargets(newCfg, previous)
			if err != nil {
				logger.Error("reload rejected, keeping the running configuration", "error", err)
				continue
			}

			runner.Stop()
			mu.Lock()
			*current = newTargets
			mu.Unlock()
			runner.Start(ctx, toSchedule(newTargets), handle)
			logger.Info("configuration reloaded", "targets", len(newTargets))
		}
	}
}

// buildTargets builds the runtime for every configured target, reusing the
// previous runtime of any target whose configuration did not change. Without that
// reuse a reload would reset every state machine (a spurious UNKNOWN -> DOWN) and
// zero every uptime window, on every config edit.
func buildTargets(cfg *config.File, previous map[string]*targetRuntime) (map[string]*targetRuntime, error) {
	out := make(map[string]*targetRuntime, len(cfg.Probes))
	for i := range cfg.Probes {
		p := cfg.Probes[i]
		if prev, ok := previous[p.Name]; ok && reflect.DeepEqual(prev.cfg, p) {
			out[p.Name] = prev
			continue
		}
		pr, err := probe.New(&p)
		if err != nil {
			return nil, fmt.Errorf("target %q: %w", p.Name, err)
		}
		out[p.Name] = &targetRuntime{
			cfg:     p,
			prober:  pr,
			machine: state.New(p.ConfirmDown, p.ConfirmUp),
			buckets: uptime.New(),
		}
	}
	return out, nil
}

func toSchedule(targets map[string]*targetRuntime) []schedule.Target {
	out := make([]schedule.Target, 0, len(targets))
	for name, rt := range targets {
		out = append(out, schedule.Target{
			Name:     name,
			Prober:   rt.prober,
			Interval: rt.cfg.Interval.D(),
		})
	}
	return out
}

func metricsMux(m *metrics.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// configHash identifies the configuration to the viewer without shipping it, so a
// changed target set is visible there.
func configHash(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// toModelResult converts an internal probe result into the wire format, carrying
// the damped state so the viewer never has to re-derive flap policy.
func toModelResult(res probe.Result, st state.State) model.Result {
	out := model.Result{
		Target:     res.Target,
		At:         res.At,
		OK:         res.OK,
		State:      string(st),
		DurationMS: res.DurationMS,
		StatusCode: res.StatusCode,
		ErrorClass: string(res.ErrorClass),
		Error:      res.Error,
	}
	if len(res.PhaseMS) > 0 {
		out.PhaseMS = make(map[string]int64, len(res.PhaseMS))
		for phase, ms := range res.PhaseMS {
			out.PhaseMS[string(phase)] = ms
		}
	}
	if !res.TLSExpiry.IsZero() {
		expiry := res.TLSExpiry
		out.TLSExpiry = &expiry
	}
	return out
}
