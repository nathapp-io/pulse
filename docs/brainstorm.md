# pulse — design brainstorm

Date: 2026-11-16
Status: proposal, awaiting decisions (see [Open questions](#12-open-questions))

## 1. Problem

There is no black-box monitoring today. Kubernetes liveness/readiness only tells you a
pod answers; it does not tell you that a public hostname answers, that TLS is about to
expire, that a Postgres port is reachable from the LAN, or that yesterday the API was
down for 20 minutes. Incidents are currently found by users.

Goal: a small, owned tool that answers *"is it up, since when, from where, and how
fast"* for a configurable list of targets.

## 2. Requirements

### Stated
- **Exporter** — probes health over TCP/HTTP(S), **multiple configurable entries**,
  exports metrics/status.
- **Viewer** — displays the collected metrics/status.
- Dockerizable, lightweight.

### Implied (from the environment and how these tools are actually used)
- **Per-target config**: interval, timeout, expectations, headers/auth.
- **Error classification**, not just up/down — DNS vs connect vs TLS vs status vs
  assertion failure. "down" alone sends you looking in the wrong place.
- **Uptime over time** (rolling windows) and **latency history** for the UI.
- **TLS certificate expiry** — the cheapest win available for HTTPS targets.
- **Flap damping** — a target goes DOWN after N consecutive failures, not one.
- **Runs next to its targets**. With 2× LAN k3s + 1× OCI k8s, a central prober cannot
  reach private addresses (`192.0.2.10:5432`) in every network. ⇒ *many probers, one
  viewer*.
- **Prometheus-compatible `/metrics`** — the platform plan already targets
  kube-prometheus-stack + Grafana "later"; the exporter should slot in without
  rewriting it.

### Explicit non-goals for v1
- Log collection (that is Loki), metrics scraping of other exporters (`node_exporter`),
  distributed tracing.
- Long-term time-series storage — Prometheus owns that. `pulse` keeps only a bounded
  in-memory window.
- Synthetics with browser rendering (that is Playwright territory).

## 3. Build vs. adopt — the honest comparison

| Option | What it is | Why not just use it |
|---|---|---|
| **Uptime Kuma** | Node + SQLite, rich UI, notifications, the de-facto self-hosted uptime tool | 150–300 MB RAM, a DB you must back up, no Prometheus exposition, awkward to run one instance per cluster |
| **Gatus** (Go) | Very close to this design: Go, YAML, endpoints + UI + alerts | Almost exactly what we are describing — worth a 1-week trial before writing code; the main reasons to build are the Prometheus `/metrics` contract, multi-prober aggregation, and full ownership of the alert path |
| **blackbox_exporter** | Prometheus sidecar prober, canonical | No UI, no history without Prometheus, config in `prometheus.yml` crammed into query params — not a "view the status" product |
| **Prometheus + alertmanager + Grafana** | The platform standard | Correct long-term target, but heavy for "is it up": 3 components + storage + alert rules before the first useful screen |

**Recommendation:** build it, because the deliverable is small (two Go binaries, no DB)
and the two integration points we actually need — Prometheus exposition *and* a
standalone viewer across clusters — are exactly what off-the-shelf tools each do only
half of. But keep the Gatus trial as a fallback if v1 slips past ~2 weeks: the risk
being managed is "we rebuilt Gatus with fewer features", and the mitigation is keeping
the exporter's `/metrics` contract standard so a swap stays possible.

## 4. Language choice

The deciding constraint is not raw speed — every probe is I/O-bound (DNS, TCP, TLS,
HTTP). It is *footprint per container, concurrency ergonomics, and image size*.

| | **Go** | **Rust** | **Node / Bun** |
|---|---|---|---|
| Binary / image size | 15–25 MB, static, distroless/scratch | 8–15 MB | 90–180 MB (runtime + deps) |
| RSS at ~100 targets | 25–40 MB | 15–25 MB | 80–150 MB |
| Concurrency model | goroutine per target — the natural fit | tokio; correct but more ceremony | event loop — fine at this scale |
| Prometheus client | `prometheus/client_golang` (first-party) | `prometheus` crate (3rd party) | 3rd party |
| Cross-compile linux/arm64 for OCI | `GOARCH=arm64`, no toolchain juggling | good, rustup targets | needs per-arch base image + native deps |
| Dev velocity (config parsing, HTTP, TLS, tests) | high | medium (borrow checker, async lifetimes) | high |
| Ecosystem prior art to borrow ideas from | blackbox_exporter, Gatus, Prometheus itself | limited | Uptime Kuma |
| Ops story | one file, `docker run`, no runtime | one file | runtime inside image |

**Verdict: Go for both components, vanilla JS + uPlot embedded for the UI.**

- Rust wins only on memory, by ~10–20 MB per container, and loses on the thing this
  project needs most: fast iteration on probe logic and an official metrics client.
  There is no CPU-bound hot path to optimise.
- Node/Bun is the right choice if the UI is the hard part — it isn't here (a target
  grid + one chart), and the per-container RSS is 3–5× Go for no benefit.
- Keeping the UI as **static assets embedded with `go:embed`** avoids an npm build
  step, a `node_modules` tree, and a frontend toolchain — the single biggest source of
  "lightweight" drift in this kind of project.

**Local prerequisite:** neither `go` nor `rustc` is installed on this machine (Node
v26 / Bun 1.4.5 are). Implementation needs `brew install go` (network access) or a
`golang:1.24` container as the build environment.

## 5. Architecture

Two binaries, one repo, shared `internal/` packages.

### 5.1 Prober (exporter)

- **Config**: single YAML file, `--config`, hot reload on `SIGHUP` + file watch.
  Invalid config never breaks a running process — keep last-good, log the diff.
- **Scheduling**: one worker goroutine per target, ticker with jitter (±10%) to avoid
  thundering herds; a global semaphore caps in-flight probes.
- **Probe types**: `http`/`https`, `tcp`, `icmp` (opt-in; needs `NET_RAW`), later `dns`,
  `grpc`.
- **Assertions** (http): status-code set, body regex, JSON path equals/contains,
  response-header check, `tls_expiry_min`.
- **Error classes**: `OK`, `DNS_FAIL`, `CONNECT_REFUSED`, `CONNECT_TIMEOUT`,
  `TLS_HANDSHAKE`, `TLS_EXPIRED`, `HTTP_TIMEOUT`, `HTTP_STATUS`, `ASSERT_FAIL`,
  `BODY_TOO_LARGE`.
- **Storage**: per-target ring buffer, default 1440 results (~92 KB/target at 30s ⇒
  12 h), plus pre-aggregated 1m/5m/1h buckets for long-range uptime without unbounded
  memory. 100 targets ≈ 9–12 MB.
- **State machine**: `UNKNOWN → UP ⇄ DOWN`, DOWN requires `confirm_down` consecutive
  failures; recovery requires `confirm_up`; maintenance windows suppress state changes
  and alerting but still record probe results.
- **Notifier** (phase 3): state transitions → generic webhook (JSON), Slack, Telegram.

### 5.2 Viewer

- Polls `N` probers' `/api/v1/status` (interval configurable, default 10s), keeps the
  aggregate in memory, serves the UI.
- **Why a separate process** even though it could live in the prober: one viewer must
  fan in probers from different networks/clusters, and the viewer is the only component
  that needs auth, a public hostname and a stable URL.
- UI (v1): target grid grouped by `group` with status pill, uptime % (1h/24h/7d), p50/p95
  latency, sparkline, TLS days-left, last error class, incident timeline.
- Optional later mode: PromQL backend when `PROMETHEUS_URL` is set — the viewer becomes
  the single ops dashboard on top of the planned kube-prometheus-stack.

### 5.3 Contracts

`GET /metrics` (Prometheus text) — one series per target:

```
probe_success{target,group,type}                    0|1
probe_duration_seconds{target,group,type,phase}      dns|connect|tls|ttfb|total
probe_http_status_code{target,group,type}
probe_tls_expiry_seconds{target,group,type}          absolute unix ts
probe_last_success_timestamp_seconds{target,group,type}
probe_error_total{target,group,type,class,phase}
probe_uptime_ratio{target,group,window}              1h|24h|7d
pulse_probes_total / pulse_prober_build_info
```

`GET /api/v1/status` (JSON, consumed by the viewer):

```json
{
  "prober": {"name": "prober-lan-a", "version": "0.1.0", "uptime_s": 86400},
  "generated_at": "2026-11-16T02:15:00Z",
  "targets": [{
    "name": "api", "group": "web", "type": "http",
    "state": "UP", "since": "2026-11-15T21:03:11Z",
    "last": {"at": "...", "ok": true, "duration_ms": 412, "status_code": 200,
             "error_class": "OK", "tls_expiry": "2027-02-04T00:00:00Z"},
    "uptime": {"1h": 1.0, "24h": 0.9986, "7d": 0.9991},
    "latency_ms": {"p50": 380, "p95": 910},
    "sparkline": [0,0,1,1,1,0,1]
  }]
}
```

`GET /api/v1/history?target=&window=` — arrays for charts. `GET /healthz`, `/readyz`.

## 6. Docker & operations

- Multi-stage: `golang:1.24-alpine` build → `gcr.io/distroless/static-debian12:non-root`
  (brings a CA bundle and a non-root `/etc/passwd` — needed for HTTPS probes).
- Expected: ~15–20 MB per image, non-root, read-only rootfs, `no-new-privileges`,
  `HEALTHCHECK` hitting `/healthz`.
- `compose.yaml` for standalone use, `config.example.yaml` mounted read-only.
- Helm chart in a later phase, matching the house Helm chart conventions
  conventions. Placement rule: **prober → same cluster/network as its targets;
  viewer → one instance, exposed through Traefik with OIDC in front.**

## 7. Risks

| Risk | Mitigation |
|---|---|
| Reinventing Gatus with fewer features | v1 scope fixed at 5 milestones; standard `/metrics` contract keeps swap possible; measure against Gatus early |
| ICMP needs elevated caps (`NET_RAW`) | opt-in flag; default probes are unprivileged |
| In-memory history lost on restart | documented; optional SQLite (phase 4) or Prometheus for durable history |
| Probe results mistaken for user-facing truth | viewer shows *from where* (prober name/region) and error class; never a bare green dot |
| Alert fatigue | confirm_up/confirm_down damping + maintenance windows before notifiers ship |
| Sandbox/network here blocks `brew`/`docker pull` | implementation commands may need to run unsandboxed or on the target host |

## 8. Open questions

1. **Alerting in v1?** Webhook/Slack/Telegram on day one, or UI-only first?
2. **Durable history?** In-memory only (restart loses it) vs SQLite vs Prometheus-only?
3. **Viewer auth?** None (internal only) / basic auth / OIDC via your IdP?
4. **Scale target?** ~10 targets, ~100, ~1000? (sets the memory/ring-buffer defaults)
5. **Prober placement?** One per cluster (3), or one central with LAN/VPN reach?
6. **ICMP needed at all?** It costs a capability; TCP connect often covers "reachable".
7. **Repo home?** GitLab `nathapp/…` like the other services, GitHub `skywills/…`, or
   local-only for now?
