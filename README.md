# pulse

Lightweight uptime & health monitoring: a **prober** (exporter) that actively probes
HTTP(S)/TCP endpoints and publishes metrics as JSON + Prometheus text, and a **viewer**
that displays the collected status.

```
 targets (https / http / tcp / icmp)
      ▲   ▲   ▲
      │   │   │   active probes, per-target interval + jitter
 ┌────┴───┴───┴─────────┐          ┌──────────────────────┐
 │  pulse-prober        │  JSON    │  pulse-viewer        │
 │  one per network     ├─────────►│  aggregates N probers│
 │  ring buffers, no DB │◄─────────┤  embedded UI (uPlot) │
 │  /metrics /api/v1/*  │  poll    │  no DB, no build step│
 └──────────┬───────────┘          └──────────────────────┘
            │ /metrics (Prometheus text format)
            ▼
   Prometheus + Grafana (optional, later)
```

- **Exporter** (`pulse-prober`): config-driven, concurrent, single static binary,
  ~15–25 MB image, ~25–40 MB RSS at 100 targets.
- **Viewer** (`pulse-viewer`): polls one or more probers, serves a static UI from the
  binary. No database, no npm build toolchain.
- **Docker-first**: distroless non-root images, `compose.yaml` for local/standalone,
  Helm chart later for the k3s/k8s clusters.

## Status

Design phase — **no application code yet**. See:

- [`docs/brainstorm.md`](docs/brainstorm.md) — requirements, buy-vs-build, language
  choice (Go), architecture, metrics/API contracts, risks.
- [`docs/plan.md`](docs/plan.md) — phased plan, repo layout, acceptance criteria.
- [`config.example.yaml`](config.example.yaml) — the probe configuration shape.

## Naming

Working name `pulse`. Rename is cheap (`mv` + sed) if you prefer something else —
alternatives that were considered: `beacon`, `vigil`, `checkmate`.

## License

TBD.
