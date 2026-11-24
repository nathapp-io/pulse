# pulse — design brainstorm

Date: 2026-11-16
Status: v1 design settled (see [Decisions](#11-decisions)); open questions in
[§12](#12-open-questions)

## 1. Problem

There is no black-box monitoring today. Kubernetes liveness/readiness only proves a pod
answers; it does not prove that a public hostname answers, that TLS is not about to
expire, that a Postgres port is reachable from the LAN, or that the API was down for 20
minutes yesterday. Incidents are currently discovered by users.

Goal: a small, owned tool that answers *"is it up, since when, from where, and how
fast"* for a configurable list of targets.

## 2. Requirements

### Stated
- **Exporter** — probes health over TCP/HTTP(S), **multiple configurable entries**,
  exports metrics/status. **Pushes** results to the viewer.
- **Viewer** — displays collected status, **never accesses an exporter directly**.
- Dockerizable, lightweight.
- **v1 UI-only** — no alerting.
- Viewer login: **Basic auth configured via environment**.
- Prober placement: decided at deployment time.

### Implied
- **Per-target config**: interval, timeout, expectations, headers/auth.
- **Error classification**, not just up/down — DNS vs connect vs TLS vs status vs
  assertion failure. "down" alone sends you looking in the wrong place.
- **Uptime over rolling windows** and **latency history** for the UI.
- **TLS certificate expiry**, the cheapest win available on HTTPS targets.
- **Flap damping** — DOWN after N consecutive failures, not one.
- **Prometheus-compatible `/metrics`** on the prober, because the platform plan already
  targets kube-prometheus-stack + Grafana "later". Independent of the push path.

### Non-goals for v1
- Alerting/notifications (explicitly deferred — interface only).
- Log collection (Loki), scraping other exporters (`node_exporter`), tracing.
- Long-term TSDB: the viewer keeps a bounded local history; Prometheus owns the rest.
- Browser-rendered synthetics.

## 3. Build vs. adopt

| Option | What it is | Why not just use it |
|---|---|---|
| **Uptime Kuma** | Node + SQLite, rich UI + notifications | 150–300 MB RAM, pull-only, no Prometheus exposition, one instance per network |
| **Gatus** (Go) | Closest prior art: Go, YAML, endpoints + UI | ~90% of this design. Legitimate fallback if v1 slips; reasons to build are the push model (NAT-friendly, zero inbound on probers), the Prometheus contract, and owning the code |
| **blackbox_exporter** | Prometheus sidecar prober | Pull-only (needs inbound reach to every target's network), no UI, no history without Prometheus |
| **Prometheus + Alertmanager + Grafana** | The platform standard | Correct long-term destination, but 3 components + storage + rules before the first useful screen |

**Recommendation: build**, but keep the `/metrics` contract standard so a future swap to
Gatus/blackbox_exporter stays possible.

## 4. Language choice

Probes are entirely I/O-bound, so the decision is footprint and iteration speed, not
throughput.

| | **Go** ✅ | Rust | Node / Bun |
|---|---|---|---|
| Image size | 15–25 MB static, distroless | 8–15 MB | 90–180 MB (runtime + deps) |
| RSS @ ~100 targets | 25–40 MB | 15–25 MB | 80–150 MB |
| Concurrency | goroutine per target — natural fit | tokio, more ceremony | event loop, fine at this scale |
| Prometheus client | `client_golang` (first-party) | 3rd party | 3rd party |
| arm64 for cloud nodes | `GOARCH=arm64`, no toolchain juggling | good | needs per-arch base image |
| **CGO-free SQLite** | `modernc.org/sqlite` keeps distroless/static | `rusqlite` usually needs CGO or a C toolchain | `node:sqlite` fine but heavy image |
| Dev velocity (config, HTTP, TLS, SQL, tests) | high | medium | high |

**Verdict: Go for both binaries, vanilla JS + uPlot embedded for the UI.**

- Rust wins ~10–20 MB RSS and loses iteration speed on a codebase with no CPU hot path.
- Node/Bun is right only if the UI is the hard part; here it is a target grid and one
  chart. It also costs 3–5× the RSS per container.
- Embedding the UI with `go:embed` avoids npm/node_modules/a frontend toolchain — the
  main way "lightweight" drifts in projects like this.

**Local prerequisite:** neither `go` nor `rustc` is installed on this machine (Node v26 /
Bun 1.4.5 are). Implementation needs `brew install go` (network) or a `golang:1.24`
build container.

## 5. Architecture — why push changes more than the arrow direction

### 5.1 What push buys
- Probers need **no inbound rule, no public hostname, no TLS cert** — they work from a
  NAT'd LAN cluster with egress only.
- The **trust boundary is one component**: the viewer. Only it is exposed.
- The viewer does not need network reachability *into* every cluster, which is what makes
  the "LAN k3s + cloud k8s" topology cheap.

### 5.2 What push costs — and how each cost is paid
| Push failure mode | Handling |
|---|---|
| Viewer unreachable | Prober keeps probing and buffers results in a **bounded FIFO** (default 10k); exponential backoff + jitter; drops oldest first and increments `pulse_push_dropped_total`. Probing never blocks on the network. |
| Viewer restarts | Queue drains; ingest is **idempotent** on `(prober, target, at)` so retries cannot double-count. |
| Prober dies — *silence is ambiguous* | Viewer tracks `last_seen` per prober; targets go **`STALE`** (grey, not red) after `3 × push_interval`, and viewers listing `PULSE_EXPECTED_PROBERS` report a prober that has never checked in. Silence must be visible, not absent. |
| Target removed from prober config | Prober reports its `config_hash` + target list; the viewer archives targets that disappeared instead of showing them stale forever. |
| Clock skew / lag | Each result carries prober time; the viewer also stores `received_at` and surfaces ingest lag in the UI. |
| One prober forging another's data | v1: single shared ingest token. Phase 4: per-prober tokens. |

### 5.3 Components

**Prober (exporter)**
- YAML config, `--config`, reload on `SIGHUP` / file change; invalid config keeps the
  last-good state and never stops the process.
- One worker goroutine per target, ticker with ±10% jitter, global in-flight semaphore.
- Probe types: `http`/`https`, `tcp`, `icmp` (opt-in, needs `NET_RAW`); later `dns`.
- Assertions (http): status set, body regex, JSON path, header check, `tls_expiry_min`.
- Error classes: `OK`, `DNS_FAIL`, `CONNECT_REFUSED`, `CONNECT_TIMEOUT`, `TLS_HANDSHAKE`,
  `TLS_EXPIRED`, `HTTP_TIMEOUT`, `HTTP_STATUS`, `ASSERT_FAIL`, `BODY_TOO_LARGE`.
- State machine `UNKNOWN → UP ⇄ DOWN` with `confirm_down`/`confirm_up` damping.
- Holds **only** current state + uptime buckets (1m × 360 for 1h, 1h × 168 for 7d
  ≈ 20 KB/target) so `/metrics` stays meaningful without a database.
- Push client: batch every `push_interval` (default 10s) **plus immediately on a state
  transition** (so a DOWN appears in seconds, not up to 10s later).
- `GET /metrics`, `GET /healthz` on a **separate port** (`:8081`), for Prometheus later.

**Viewer**
- `POST /api/v1/ingest` — bearer token, batched, idempotent.
- **SQLite** via `modernc.org/sqlite` (pure Go → static distroless image preserved):
  `results` (raw, default 7d), `rollups_1m` (30d), `rollups_1h` (365d), `probers`,
  `targets`. WAL mode, single writer goroutine, batched inserts.
- Staleness/`last_seen` accounting, expected-prober list, ingest-lag metrics.
- `GET /api/v1/status`, `/api/v1/history` for the UI; Basic auth on all of it.
- Optional `GET /metrics` so the viewer itself can be scraped.
- UI: static assets embedded, uPlot vendored, no CDN.

### 5.4 Storage sizing (viewer) — scale is settled at under 100 targets (D6)
At 100 targets × 30s intervals: 288k results/day, i.e. **~3.3 writes/second**. Raw rows
~100–150 B → ~30–40 MB/day, so **7 days ≈ 250 MB**; rollups are negligible (~1000× fewer
rows). All retention is configurable and a 30s→60s interval halves it.

Under 100 targets, that volume is small enough to make several things deliberately
simple: **one** SQLite writer goroutine with batched inserts (no pooling, no partitioning,
no sharding), plain `DELETE ... WHERE ts < ?` retention instead of a job queue, and whole
`/api/v1/status` responses assembled in memory. The design still carries headroom to a few
hundred targets unchanged, so growth is not a rewrite — just stop batching so aggressively.

### 5.5 Contracts

**Push payload** — `POST /api/v1/ingest`, `Authorization: Bearer <PULSE_INGEST_TOKEN>`,
response `204`:

```json
{
  "prober": {"name": "prober-lan-a", "version": "0.1.0", "labels": {"region": "lan-a"}},
  "config_hash": "sha256:9f2c…",
  "targets": [{"name": "api", "group": "web", "type": "http"}],
  "results": [{
    "target": "api",
    "at": "2026-11-16T02:15:00Z",
    "ok": true,
    "duration_ms": 412,
    "phase_ms": {"dns": 3, "connect": 12, "tls": 40, "ttfb": 350},
    "status_code": 200,
    "error_class": "OK",
    "tls_expiry": "2027-02-04T00:00:00Z"
  }]
}
```

**Viewer status API** — `GET /api/v1/status` (Basic auth):

```json
{
  "generated_at": "2026-11-16T02:15:10Z",
  "probers": [{"name": "prober-lan-a", "last_seen": "...", "lag_ms": 180,
               "state": "HEALTHY", "targets": 12}],
  "targets": [{
    "name": "api", "group": "web", "prober": "prober-lan-a",
    "state": "UP", "since": "2026-11-15T21:03:11Z",
    "last": {"at": "...", "ok": true, "duration_ms": 412, "error_class": "OK",
             "tls_expiry": "2027-02-04T00:00:00Z"},
    "uptime": {"1h": 1.0, "24h": 0.9986, "7d": 0.9991},
    "latency_ms": {"p50": 380, "p95": 910},
    "sparkline": [0, 0, 1, 1, 1, 0, 1]
  }]
}
```

**Prober metrics** — `GET /metrics` (Prometheus text), per target:

```
probe_success{target,group,type}                     0|1
probe_duration_seconds{target,group,type,phase}      dns|connect|tls|ttfb|total
probe_http_status_code{target,group,type}
probe_tls_expiry_seconds{target,group,type}
probe_last_success_timestamp_seconds{target,group,type}
probe_error_total{target,group,type,class,phase}
probe_uptime_ratio{target,group,window}              1h|7d
pulse_push_queued / pulse_push_dropped_total / pulse_push_last_success_timestamp_seconds
pulse_build_info
```

## 6. Docker & operations

- Multi-stage `golang:1.24-alpine` → `gcr.io/distroless/static-debian12:non-root`
  (CA bundle + non-root `/etc/passwd` needed for HTTPS probes).
- Non-root, read-only rootfs, `no-new-privileges`, `HEALTHCHECK` on `/healthz`.
- Prober: config mounted `:ro`, **no exposed ports required** (metrics port optional).
- Viewer: SQLite on a named volume (`/var/lib/pulse`), Basic auth from env, behind TLS
  (Traefik) — Basic auth without TLS is documented as unsupported.
- Helm chart later; NetworkPolicy shape: prober **egress** to viewer + targets, viewer
  **ingress** from probers and from the UI users.

## 7. Risks

| Risk | Mitigation |
|---|---|
| Rebuilding Gatus with fewer features | Fixed v1 scope (5 milestones), standard `/metrics` contract, early comparison against Gatus |
| Silent data loss when the viewer is down | Bounded queue + drop counter + `pulse_push_dropped_total` alertable later |
| "Green dot" mistaken for user-facing truth | UI always shows *which prober* and the error class; `STALE` is a distinct state |
| SQLite growth | Retention defaults + rollups + documented size math; WAL, single writer |
| Viewer single point of failure for display | It is read-only history; probers keep probing and buffering, no data beyond the queue is lost |
| ICMP needs `NET_RAW` | Opt-in; default probes unprivileged |
| Basic auth over plain HTTP | Documented as requiring TLS termination in front; OIDC in phase 4 |

## 8. v1 scope (agreed)

In: prober (http/tcp, assertions, classification, damping), push client, viewer ingest,
SQLite history + rollups, staleness detection, viewer UI, Basic auth, Docker/compose.

Out (post-v1 backlog): alerting/notifiers, OIDC, per-prober tokens, ICMP default, DNS/
gRPC probes, PromQL backend, multi-tenant, SLA reports/exports.

## 11. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | v1 UI-only, no alerting | Keep v1 small; notifier interface stubbed so phase 3 does not need a redesign |
| D2 | Push only; viewer never contacts a prober | NAT'd LAN clusters need no inbound rules; only one component exposed |
| D3 | **History in the viewer** (SQLite) | Probers become disposable and stateless-ish; one history for N probers; a prober restart loses nothing; no per-prober volume to back up |
| D4 | Basic auth from env | Simplest thing that is not "no auth"; OIDC later |
| D5 | Placement decided per deployment | Prober name/labels travel in the payload, so nothing assumes a topology |
| D6 | **Under 100 targets** | ~3.3 probe results/s: one SQLite writer + batched inserts, in-memory status assembly, simple retention. Keeps the whole stack in tens of MB of RAM and ~250 MB of disk |

D3 in more detail: the prober could keep history, but then the viewer would have to
**pull** it (contradicting D2), or each prober would keep its own history and the viewer
would only see the latest push — losing uptime/latency windows. Storing in the viewer is
the only option consistent with push. The prober therefore keeps just enough local state
for its own `/metrics` (~20 KB/target), and that is explicitly *not* the source of truth.

## 12. Open questions

None of these block M1/M2 — defaults are in the config examples and are cheap to change.

1. **`/metrics` on the prober — keep or drop?** Assumed **kept**, on a separate port, for a
   future Prometheus; the viewer never uses it. Say the word and it becomes opt-in.
2. **Retention** — assuming raw 7d / 1m 30d / 1h 365d. At <100 targets this holds under
   ~250 MB, which is why the default is generous.
3. **Ingest token** — one shared token for v1 (assumed), per-prober tokens in M4.
4. **Repo home** — Resolved: public GitHub repository `nathapp-io/pulse`.
