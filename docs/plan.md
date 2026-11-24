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

## M2b — Prober push client ✅ done (2026-11-24)

Bounded FIFO queue (drops the **oldest** when full, because the newest samples are what
the UI shows as current), batching every `push_interval`, **immediate flush on a state
transition**, exponential backoff with jitter on failure, flush-on-shutdown, and
`Dockerfile` + `compose.yaml` for the two-service stack.

Delivery is at-least-once: a batch stays queued until the viewer accepts it, and ingest is
idempotent on `(prober,target,at)`, so a retry stores nothing twice.

**Acceptance (met, real processes):**
- Viewer down 20s while the prober kept probing: queue grew to 10, `pulse_push_dropped_total`
  stayed **0**, 4 failed attempts with backoff, then everything drained on recovery.
- After recovery: **17 samples over a 33.6s span, maximum gap 2.2s** at a 2s interval —
  no hole where the outage was. A naive implementation shows a ~20s gap here.
- Unit tests pin the rest: an immediate flush beats a one-hour interval, a 500 is retried
  until accepted, a cancelled run still flushes, and a full queue drops the oldest.
- Image builds from `Dockerfile` with the Go 1.21 build stage, proving the floor claim.

## M3 — Viewer UI ✅ done (2026-11-24)

Dark, responsive, embedded in the binary: no CDN and no npm build step, so the UI adds no
second supply chain. `internal/ui` keeps the assets behind `//go:embed`; uPlot is vendored
with its MIT licence.

- Summary counters: up / down / stale / unknown / targets / probers / missing.
- Prober strip: state, last seen, version. A declared-but-never-seen prober shows
  MISSING, which is the case that would otherwise look like "no problem here".
- Target cards: state pill, 1h/24h/7d uptime bars, p50/p95 over the last hour, owning
  prober, and the last error class and message.
- Clicking a card opens its latency chart for the last 6h from `/api/v1/history`; failed
  probes are gaps in the line rather than zero, so an outage cannot look like a fast
  response.
- Auto-refresh every 10s (chart every 30s), paused while the tab is hidden.
- Same Basic auth as the API on both the page and the assets, plus
  `Content-Security-Policy`, `X-Content-Type-Options: nosniff`, and `no-cache` on the page.
  Every API string is escaped before it reaches the DOM.

**Acceptance (met, in real containers):** page weight **28.3 KB gzipped** (budget 100 KB);
page and assets 200 behind auth and 401 without it; unknown paths 404 instead of rendering
the page; on the compose stack both demo targets render UP at 100% 1h uptime, the prober
shows HEALTHY, and the history endpoint returns the samples the detail chart plots.

**Deferred:** grouping cards by `group`, TLS days-left, an incident timeline, 7d rollup
charts, a light theme, and gzip (responses are served identity-encoded).

## M4 — Maintenance windows, ingest auth, retention ✅ done (2026-11-24)

Assertions (`status`, `body_regex`, dotted `json_path`, header regex, `tls_expiry_min`) and
retention pruning of raw rows plus 1m/1h rollups already existed from M1/M2; this
milestone closed the two gaps that made them unusable in practice, and verified the rest
end to end.

**Maintenance windows.** `maintenance:` on a probe is now honoured instead of parsed and
ignored. The prober flags each result taken inside a window; the viewer shows the target
as **MAINTENANCE** and stops counting it as DOWN. The raw result is not rewritten: history
still records that the probe failed, so a planned outage is not hidden from a later
investigation — only from the alarm counters. STALE outranks MAINTENANCE, because when the
prober itself is silent, freshness is the more important fact.

**Per-prober ingest tokens.** `PULSE_INGEST_TOKENS` binds a token to a prober name, so a
token leaked from one prober cannot fabricate history for another. `PULSE_INGEST_TOKEN`
still works as one shared token. `PULSE_INGEST_TOKENS_FILE` is re-read every 2s, because a
revoked token has to stop working without restarting the viewer. A file that becomes
unreadable or malformed keeps the **last good** token set and logs a warning — a typo in
that file must not lock every prober out.

**Storage.** SQLite gained a `results.maintenance` column with an idempotent `ALTER TABLE`
migration, so a database created by an earlier version opens and keeps working.

**Acceptance (met, real processes):**
- A target returning **200 with a body that fails its regex** reports `ASSERT_FAIL` in the
  status API and therefore in the UI.
- A target inside its window, whose endpoint is actually refusing connections, reports
  **MAINTENANCE**: `down` stays 0 for it, `maintenance` is 1, and its 5 recorded history
  samples are flagged `maintenance: true` while still `ok: false`.
- Editing the tokens file revokes a token: old token 401, rotated token 204, and a valid
  token presented under a different prober name 401. The running prober's
  `pulse_push_failures_total` climbs within one push interval, and its queue holds the
  results rather than losing them.

**Deferred:** OIDC and the optional PromQL (`PROMETHEUS_URL`) history backend. Both need a
real IdP / Prometheus to test against honestly, and neither is needed to run the stack.

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
