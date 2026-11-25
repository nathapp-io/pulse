# pulse

Lightweight uptime & health monitoring, **push-based**: a **prober** (exporter) actively
probes HTTP(S)/TCP targets and pushes results to a **viewer**, which owns the history and
serves the UI. The viewer never connects back to a prober.

```
 targets (https / http / tcp / icmp)
      ▲   ▲   ▲
      │   │   │  active probes, per-target interval + jitter
 ┌────┴───┴───┴──────────┐    POST /api/v1/ingest      ┌───────────────────────┐
 │  pulse-prober         │    bearer token, batched    │  pulse-viewer         │
 │  one per network      │  ─────────────────────────► │  history: memory or   │
 │  OUTBOUND ONLY (no    │    retry + backoff queue    │  SQLite, embedded UI  │
 │  inbound rule needed) │                             │  Basic auth from env  │
 │  /metrics on :8081    │                             │  /metrics (optional)  │
 └───────────┬───────────┘                             └───────────────────────┘
             │ /metrics (Prometheus text, for future Prometheus — never the viewer)
             ▼
    Prometheus + Grafana (later)
```

- **Prober** (`pulse-prober`): config-driven, concurrent, single static binary,
  ~15–25 MB image. Holds only the current state + small uptime buckets for its own
  `/metrics`; ships every result to the viewer.
- **Viewer** (`pulse-viewer`): ingest API + **pluggable history** (`memory` or `sqlite`,
  pure-Go driver so no CGO) + static UI embedded in the binary. No npm build step, no
  external database.
- **UI**: dark, responsive, embedded in the viewer binary — no CDN, no npm build step.
  Summary counters, per-prober health, target cards with 1h/24h/7d uptime bars and
  p50/p95, and a latency chart per target from the history API. **28 KB gzipped** in
  total. Behind the same Basic auth as the API.
- **Docker-first**: distroless non-root images, `compose.yaml` for a two-service stack,
  and a Helm chart with NetworkPolicies that give the prober no inbound rule at all.

Why push: probers sit inside LAN clusters behind NAT and need **zero inbound rules**;
the viewer is the only thing exposed, so the attack surface and the trust boundary are
one component wide.

## Status

**M1 (prober core), M2a (viewer ingest + pluggable store), M2b (prober push client,
Docker, compose), M3 (the UI), M4 (maintenance windows, per-prober tokens) and M5 (Helm
chart, CI, runbook) are implemented and tested.**

Storage is pluggable per deployment: `memory` keeps bounded rings in RAM (zero setup,
history lost on restart) and `sqlite` persists to one file (durable). `PULSE_STORE`
selects it; a `PULSE_DB` path implies sqlite, its absence implies memory.

## Quick start

```sh
cp .env.example .env      # edit the secrets
docker compose up -d --build
open http://localhost:8080/     # PULSE_PORT overrides the host port
curl -u admin:change-me http://localhost:8080/api/v1/status
```

The bundled prober probes two demo targets and pushes to the viewer every 10s, so the UI
fills in within about 30s. Replace
[`deploy/prober.compose.yaml`](deploy/prober.compose.yaml) with your own targets.

Measured 2026-11-24 against real local targets:

| Check | Result |
|---|---|
| `go vet ./...` | clean |
| `go test -race ./...` | 79 tests, 11 packages, all pass; both storage backends run the same suite |
| Static binary (macOS arm64) | 14 MB prober, no CGO |
| 100 targets @ 5s intervals | **22.8 MB RSS** (budget was 60 MB) |
| Error classification | `HTTP_STATUS`, `CONNECT_REFUSED`, `ASSERT_FAIL` all correct on live probes |
| Damping | `UNKNOWN → DOWN` only on the 2nd consecutive failure |
| `SIGHUP` reload | applies live, preserves state/uptime for unchanged targets, rejects an invalid file and keeps running |
| Config typo (`intervall:`) | rejected at startup with the line number |
| Ingest auth | wrong/missing token → 401, correct → 204, replay stores nothing |
| Read auth | 401 + `WWW-Authenticate`, plain and bcrypt both accepted |
| Silent prober | its targets go **STALE**, never DOWN, last result still shown |
| SQLite restart | 2 samples before, **2 after** |
| Memory restart | 0 samples after, by design |
| Bad config | refuses sqlite without `PULSE_DB`, refuses ingest without a token |
| **Viewer outage, 20s** | prober queued **10**, dropped **0**, retried 4× with backoff; after recovery **17 samples over a 33.6s span, max gap 2.2s** — no hole in history |
| Push on state change | flushes immediately instead of waiting for the batch interval |
| Shutdown | flushes the queue before exiting |
| UI | 200 behind auth, 401 without; unknown paths 404; **28.3 KB gzipped** in total |
| UI in containers | both demo targets render UP at 100% 1h uptime, prober HEALTHY, detail chart fed by the history API |
| Assertion failure | 200 with a body that fails `body_regex` → `ASSERT_FAIL`, not UP |
| Maintenance window | target shown **MAINTENANCE**, not counted DOWN, results still recorded and flagged |
| Token revocation | old token 401, rotated token 204, wrong prober name 401 — no restart, within the 2s TTL |
| Token binding | a token for one prober cannot write another prober's history |
| SQLite migration | a database created before `results.maintenance` existed opens and ingests |
| Helm chart | `lint` clean, every variant renders, both safety guards fire, `install --dry-run=server` accepted by a real API server |
| Chart-generated config | rendered prober config passes `pulse-prober --check` |
| Multi-arch | both roles cross-compile to static `x86-64` and `aarch64` binaries |
| CI | `actionlint` clean; the helm job's steps run green locally |

Decisions taken 2026-11-16:

| # | Decision |
|---|---|
| D1 | v1 is UI-only: no alerting. Notifier interface stubbed, no implementation. |
| D2 | **Push only.** Prober → viewer over HTTP(S). Viewer never calls a prober. |
| D3 | **History lives in the viewer**, in a **pluggable store**: `memory` (bounded rings, zero setup, lost on restart) or `sqlite` (durable file). Same interface, same API. |
| D4 | Viewer auth: **HTTP Basic from env** (`PULSE_AUTH_USER` / `PULSE_AUTH_PASSWORD`, or `PULSE_AUTH_PASSWORD_HASH` for bcrypt). |
| D5 | Prober placement is a deployment-time decision; nothing in the design assumes it. |
| D6 | Scale: **under 100 targets** — one writer, batched inserts, simple retention. |
| D7 | **Maintenance windows stop alarms, not recording.** Results inside a window are stored and flagged; the viewer shows MAINTENANCE and does not count the target DOWN. Raw results are never rewritten. |
| D8 | **Ingest tokens are per prober** (`prober=token`), and revocable from a file without a restart; one shared token stays available for simple setups. |

## Deploy

```sh
# two containers, e.g. on one host
docker compose up -d --build

# or Kubernetes
helm upgrade --install pulse deploy/helm/pulse -n monitoring --create-namespace \
  --set viewer.auth.password='...' -f my-values.yaml
```

CI publishes public multi-arch (`amd64`, `arm64`) images to
`ghcr.io/nathapp-io/pulse/{viewer,prober}`, so there is nothing to build and no pull
secret to create:

```sh
cp .env.example .env      # edit the secrets
docker compose -f deploy/compose.release.yaml --env-file .env up -d
```

The chart refuses to render when the configuration would silently lose history
(`sqlite` with more than one viewer replica, or persistence enabled with the `memory`
store). Operations, triage and rotation procedures are in
[`docs/runbook.md`](docs/runbook.md).

## Target identity and history

A target is identified by **prober name and target name**. For example,
`GET /api/v1/history?prober=edge&target=api&window=6h` returns only `edge`’s
measurements. Older clients may omit `prober` for unique names; an ambiguous
name returns HTTP 400 instead of combining measurements from different locations.

On ingest, a prober’s full `targets` list determines which of its targets appear
in status and counts. Retiring a target does not delete its history; it remains
queryable by the same pair until normal retention expires. A results-only push
that omits `targets` does not replace the declaration.

## Viewer storage

Chosen per deployment, one interface, identical API and behaviour:

| | `memory` | `sqlite` |
|---|---|---|
| Setup | none | one file (`PULSE_DB`) |
| History across restart | **lost** | kept |
| Long windows | uptime buckets in RAM | 1m/1h rollups |
| Good for | single node, demos, CI | anything you want to keep |

`PULSE_STORE=memory|sqlite` selects it. When unset, a `PULSE_DB` path implies `sqlite`
and no path implies `memory`, so `docker run` works with no configuration at all. The
backend in use is logged at startup, because silently losing history is worse than being
told.

See [`docs/brainstorm.md`](docs/brainstorm.md) for the reasoning, contracts and risks,
[`docs/plan.md`](docs/plan.md) for milestones, and
[`config.example.yaml`](config.example.yaml) / [`viewer.env.example`](viewer.env.example)
for the configuration shape.

## Naming

Working name `pulse`. Rename is cheap (`mv` + sed) if you prefer something else —
alternatives considered: `beacon`, `vigil`, `checkmate`.

## License

MIT — see [LICENSE](LICENSE).
