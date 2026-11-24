# pulse — implementation plan

Companion to [`brainstorm.md`](brainstorm.md). Push model (D2), history in the viewer
(D3), UI-only v1 (D1), Basic auth from env (D4).

## Repo layout

```
pulse/
├── cmd/
│   ├── prober/main.go          # exporter binary
│   └── viewer/main.go          # viewer binary
├── internal/
│   ├── config/                 # YAML schema, validation, hot reload
│   ├── probe/                  # http.go tcp.go icmp.go
│   ├── assert/                 # status / regex / json-path / header / tls-expiry
│   ├── classify/               # error classes from transport + assertion failures
│   ├── schedule/               # per-target workers, jitter, semaphore
│   ├── state/                  # UNKNOWN/UP/DOWN, confirm_up/down, maintenance
│   ├── push/                   # batch builder, bounded queue, backoff, client
│   ├── ingest/                 # viewer: token auth, decode, dedupe, fan-in
│   ├── db/                     # viewer: SQLite schema, migrations, retention, rollups
│   ├── staleness/              # viewer: last_seen, expected probers, STALE state
│   ├── api/                    # viewer: /api/v1/status, /api/v1/history, auth
│   ├── metrics/                # prometheus collectors for both binaries
│   └── web/                    # go:embed static UI + uPlot (vendored)
├── web/                        # index.html, app.js, style.css, vendor/uplot
├── deploy/
│   ├── compose.yaml
│   └── helm/                   # phase 5
├── config.example.yaml         # prober config
├── viewer.env.example          # viewer env (auth, retention, expected probers)
├── docs/
└── Makefile                    # build, test, lint, docker, cross-arch
```

## M0 — Skeleton

`go.mod`, both `main.go` stubs, the shared result/ingest types, config schema + strict
validation with table-driven tests, `Makefile`, `compose.yaml` skeleton (empty images).

**Acceptance:** `go build ./...` and `go test ./internal/config` pass; `pulse-prober
--config config.example.yaml --check` validates the example and exits 0; unknown YAML
keys are a hard error (typos in probe config must never silently do nothing).

## M1 — Prober core ✅ done (2026-11-24)

HTTP(S) + TCP probes, error classification, per-target scheduling with jitter and a
global semaphore, state machine with `confirm_up`/`confirm_down`, uptime buckets,
`/metrics` + `/healthz` on `:8081`.

**Acceptance (met, measured against real local targets):**
- 4 targets (healthy HTTPS-equivalent, wrong body, HTTP 500, closed TCP port) report
  `probe_success` 1/0/0/0 with `probe_error_total` classes `HTTP_STATUS`,
  `ASSERT_FAIL`, `CONNECT_REFUSED`.
- A slow-but-connected server is `HTTP_TIMEOUT`, not `CONNECT_TIMEOUT`.
- Damping: `UNKNOWN → DOWN` only on the second consecutive failure.
- 100 targets @5s: **22.8 MB RSS**, binary **14 MB**, no CGO.
- `SIGHUP` applies changes live, **preserves state and uptime for unchanged targets**
  (a reload must not reset history), and rejects an invalid file while continuing to
  serve.
- Unknown YAML keys are rejected with the line number.

## M2a — Viewer ingest + pluggable store ✅ done (2026-11-24)

Ingest endpoint (bearer token, idempotent on `(prober,target,at)`), status and history
API (Basic auth, plain or bcrypt), staleness accounting, and **two storage backends
behind one interface**: `memory` (bounded rings + uptime buckets) and `sqlite` (WAL,
`UNIQUE(prober,target,at)`, 1m/1h rollups, retention prune).

**Acceptance (met):**
- One shared test suite runs against both backends, so they cannot drift: idempotent
  replay, `Since` on transition, uptime ratios, nearest-rank p50/p95, retention prune,
  declared-but-never-probed targets visible as UNKNOWN.
- `POST /api/v1/ingest` without or with a wrong token → 401; with the right token → 204;
  replaying it inserts nothing.
- Read API without credentials → 401 + `WWW-Authenticate`; plain and bcrypt both accepted.
- Kill the prober (or stop pushing): its targets go **STALE**, never DOWN, and the last
  known result is still shown.
- `PULSE_STORE=sqlite` keeps history across a restart (2 samples before, 2 after);
  `memory` does not, by design and by documentation.
- Startup refuses `sqlite` without `PULSE_DB` and refuses to serve ingest without a token.

## M2b — Prober push client (next)

Batching, immediate send on state transition, bounded queue, exponential backoff +
jitter, drop counter, `compose.yaml` with both services.

**Acceptance:**
- Stop the viewer, let the prober run 2 minutes, start the viewer: **no gap** in history,
  `pulse_push_dropped_total` = 0.
- A state transition is visible in the viewer within one push interval, not one batch
  interval.

## M3 — Viewer UI

Grid grouped by `group`, status pill (UP/DOWN/STALE/UNKNOWN), uptime 1h/24h/7d from
rollups, latency p50/p95, sparkline, TLS days-left, incident timeline, per-prober lag and
last-seen. Dark/light, responsive, no CDN, no build step.

**Acceptance:** page weight < 100 KB gzipped; all three probers visible in compose with
correct attribution; a target killed in the UI shows DOWN with error class and "since";
a stopped prober shows STALE grey with "last seen 42s ago"; charts render 7d of rollups
without loading raw rows.

## M4 — Assertions, retention, auth hardening

Body regex / JSON path / header assertions wired end-to-end, `tls_expiry_min` warnings,
maintenance windows, retention jobs + documented sizing, per-prober ingest tokens,
optional OIDC (any OIDC IdP), optional PromQL backend (`PROMETHEUS_URL`).

**Acceptance:** a 200 with a wrong body is `ASSERT_FAIL` in the UI; a target in a
maintenance window still records results but is not counted as DOWN; retention prunes raw
rows and keeps rollups; a revoked prober token is rejected within one push interval.

## M5 — Deploy & CI

Helm chart matching the house conventions, GitLab CI (lint, `go test
-race`, buildx `linux/amd64,linux/arm64`), NetworkPolicy (prober egress → viewer ingress
only), runbook.

**Acceptance:** chart installs on a k3s cluster; arm64 images run on cloud nodes; a prober
inside the cluster reaches private `ClusterIP` targets while exposing no inbound port;
viewer Ingress behind Traefik with TLS.

## Testing strategy

- `httptest` for the HTTP probe/assertion matrix (status, redirect, TLS, timeout, slow
  body, gzip, bad JSON).
- Real `net.Listen` on `127.0.0.1:0` for TCP; a closed port for `CONNECT_REFUSED`.
- Table-driven config validation (missing interval, bad regex, duplicate names, unknown
  keys).
- Push tests against an `httptest` viewer: retry/backoff, queue overflow ordering,
  idempotent replay.
- DB tests on a temp SQLite file: migration idempotency, dedupe, rollup math, retention.
- `go test -race ./...`; one compose smoke test covering M2's acceptance list.

## Settled inputs

- Scale: **under 100 targets** (D6) — ~3.3 results/s, so M2's acceptance numbers are the
  real ones: single writer, 7d raw ≈ 250 MB, 100 targets < 60 MB RSS in the prober.
  No milestone needs sharding, partitioning or a real TSDB.
- Prober `/metrics`: kept on a separate port, unused by the viewer.

## Still needed before M1

1. A Go toolchain: neither `go` nor `rustc` exists on this machine. Either
   `brew install go` (needs network) or build in the `golang:1.24` container.
2. Repo home / remote (local-only is fine until M5).
