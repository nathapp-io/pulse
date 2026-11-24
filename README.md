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
 │  one per network      │  ─────────────────────────► │  SQLite = history     │
 │  OUTBOUND ONLY (no    │    retry + backoff queue    │  embedded UI (uPlot)  │
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
- **Viewer** (`pulse-viewer`): ingest API + SQLite (pure-Go driver, no CGO) + static UI
  embedded in the binary. No npm build step, no external database.
- **Docker-first**: distroless non-root images, `compose.yaml`, Helm chart later.

Why push: probers sit inside LAN clusters behind NAT and need **zero inbound rules**;
the viewer is the only thing exposed, so the attack surface and the trust boundary are
one component wide.

## Status

Design phase — **no application code yet**. Decisions taken 2026-11-16:

| # | Decision |
|---|---|
| D1 | v1 is UI-only: no alerting. Notifier interface stubbed, no implementation. |
| D2 | **Push only.** Prober → viewer over HTTP(S). Viewer never calls a prober. |
| D3 | **History lives in the viewer** (SQLite). Prober keeps only live state + uptime buckets. |
| D4 | Viewer auth: **HTTP Basic from env** (`PULSE_AUTH_USER` / `PULSE_AUTH_PASSWORD`). |
| D5 | Prober placement is a deployment-time decision; nothing in the design assumes it. |
| D6 | Scale: **under 100 targets** — one SQLite writer, batched inserts, simple retention. |

See [`docs/brainstorm.md`](docs/brainstorm.md) for the reasoning, contracts and risks,
[`docs/plan.md`](docs/plan.md) for milestones, and
[`config.example.yaml`](config.example.yaml) / [`viewer.env.example`](viewer.env.example)
for the configuration shape.

## Naming

Working name `pulse`. Rename is cheap (`mv` + sed) if you prefer something else —
alternatives considered: `beacon`, `vigil`, `checkmate`.

## License

TBD.
