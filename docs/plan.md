# pulse — implementation plan

Companion to [`brainstorm.md`](brainstorm.md). Each milestone is independently
shippable and ends with a runnable container.

## Repo layout

```
pulse/
├── cmd/
│   ├── prober/main.go          # exporter binary
│   └── viewer/main.go          # viewer binary
├── internal/
│   ├── config/                 # YAML schema, validation, hot reload
│   ├── probe/                  # http.go tcp.go icmp.go + error classification
│   ├── assert/                 # status/regex/json-path/tls-expiry matchers
│   ├── schedule/               # per-target workers, jitter, semaphore
│   ├── store/                  # ring buffer + 1m/5m/1h aggregates
│   ├── state/                  # UP/DOWN machine, confirm_up/down, maintenance
│   ├── exporter/               # prometheus collectors + handlers
│   ├── api/                    # JSON handlers (/api/v1/status, /history)
│   ├── notify/                 # webhook, slack, telegram (phase 3)
│   └── viewer/                 # prober clients, aggregate cache, auth
├── web/                        # embedded static UI (go:embed), uPlot vendored
├── deploy/
│   ├── compose.yaml
│   └── helm/                   # phase 5
├── config.example.yaml
├── docs/
└── Makefile                    # build, test, lint, docker
```

## M1 — Prober core (the useful part)

Config loading + validation, HTTP(S) and TCP probes, per-target scheduling with jitter,
error classification, ring buffer, `/metrics`, `/api/v1/status`, `/healthz`, distroless
image, `compose.yaml`.

**Acceptance:** given a config with 3 targets (1 healthy HTTPS, 1 closed TCP port, 1 HTTP
500), `docker run` exposes `/metrics` with `probe_success` 1/0/0 and correct
`probe_error_total{phase}` classes; image < 30 MB; 100 targets at 30s intervals stay
under 60 MB RSS; `kill -HUP` applies a config change without dropping the process.

## M2 — Viewer UI

Fan-in of N probers, status grid grouped by `group`, uptime 1h/24h/7d, latency p50/p95,
sparkline, TLS days-left, incident timeline, dark/light. Static assets embedded, no npm
in the build.

**Acceptance:** with two prober containers in compose, the viewer lists both, a killed
target turns DOWN within `confirm_down × interval` and recovers automatically; page
weight < 100 KB gzipped; works with JS enabled only (no build step, no CDN).

## M3 — Assertions, notifications, damping

Body regex + JSON path + header assertions, `tls_expiry_min` warnings, `confirm_up`/
`confirm_down`, maintenance windows, notifier interface + webhook/Slack/Telegram.

**Acceptance:** a target that returns 200 with a wrong body is ASSERT_FAIL; a 20s blip
inside a maintenance window produces zero notifications; a real outage produces exactly
one DOWN notification with error class and prober name.

## M4 — Auth & scale

Viewer auth (basic, then OIDC against your IdP), multi-prober labels/regions, optional
SQLite persistence, optional PromQL backend (`PROMETHEUS_URL`).

**Acceptance:** unauthenticated request redirected to OIDC; history survives a viewer
restart when SQLite is enabled; 1000 targets under 200 MB RSS.

## M5 — Deploy & CI

Helm chart matching the house Helm chart conventions, GitLab CI (lint, `go test
-race`, buildx multi-arch `linux/amd64,linux/arm64` push), NetworkPolicy snippet,
runbook with placement guidance (prober per cluster, viewer behind Traefik).

**Acceptance:** chart installs on a k3s cluster with an Ingress, arm64 image runs, probes
reach private `ClusterIP` services from inside the cluster.

## Testing strategy

- `httptest` servers for HTTP probe/assertion matrix (status, redirect, TLS, timeout,
  slow body, gzip, wrong JSON).
- Real `net.Listen` on `127.0.0.1:0` for TCP; closed-port test for CONNECT_REFUSED.
- Table-driven config validation (missing interval, bad regex, duplicate names).
- `go test -race ./...`; a compose smoke test for M1/M2.
- No mocks for the network layer — the probes *are* the network layer.

## Decisions needed before M1

From the brainstorm's open questions, these four block code:

1. Alerting in v1 or UI-only? (M3 assumption: UI-only, notifier interface stubbed)
2. In-memory history vs SQLite? (M1 assumption: in-memory only)
3. Viewer auth now or later? (assumption: later, internal-only)
4. Prober placement — one per cluster? (assumption: one per cluster, N probers)
