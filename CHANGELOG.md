# Changelog

Notable changes, newest first. This project follows [semantic versioning](https://semver.org/).

## Unreleased

- Target status, charts and history now distinguish probers monitoring the same target
  name. `/api/v1/history` accepts `prober`; omitting it for an ambiguous name returns 400.
- A full target declaration retires removed targets without erasing their history. Both
  stores report active targets per prober, and delayed SQLite results no longer overwrite
  a newer observation’s state.
- HTTP time-to-first-byte measurements now work on reused connections.
- A configured ingest token file is authoritative. Helm limits the prober to its own
  token and references externally managed Secrets instead of creating them. See the
  [runbook](docs/runbook.md) when upgrading an existing installation.

## 0.1.0 — 2026-11-24

First release. A push-based uptime monitor: probers dial out, the viewer owns history and
serves the UI, so a monitored network never has to accept an inbound connection.

### Components

- **prober** — HTTP(S) and TCP probes with assertions (`status`, `body_regex`, `json_path`,
  header regex, `tls_expiry_min`), error classification (`DNS_FAIL`, `CONNECT_REFUSED`,
  `TLS_EXPIRED`, `ASSERT_FAIL`, …), state damping (`confirm_up` / `confirm_down`),
  maintenance windows, a bounded push queue that drops the oldest entry and reports it,
  and SIGHUP reload that keeps probe state. Exposes `/metrics` and `/healthz`.
- **viewer** — ingest, status and history API plus an embedded UI. Pluggable history
  store (`memory` or `sqlite`), retention of raw results and of 1m/1h rollups, staleness
  handled in one place so both stores behave identically, per-prober ingest tokens that
  can be revoked from a file without a restart, and HTTP Basic auth for reads.

### UI

Single page served from the binary (`internal/ui`, no npm, no CDN): summary counters
including stale, maintenance and missing; a prober strip; target cards with 1h/24h/7d
uptime bars, p50/p95 and last error; click a card for a 6h latency chart where failures
appear as gaps rather than as zero. 28 KB gzipped.

### Deployment

- Distroless, non-root images (~19 MB) built for `linux/amd64` and `linux/arm64`, one
  Dockerfile selected with `ROLE=prober|viewer`.
- `compose.yaml` builds from source; `deploy/compose.release.yaml` runs the published
  images with no build and no registry login.
- Helm chart with NetworkPolicies that give the prober **no inbound rule at all**, and
  guards that refuse to render a configuration that would silently lose history.
- `docs/runbook.md` for operations and triage.

### Deliberate omissions

- **No alerting.** This release is a UI and an API; a notifier is the next milestone.
- **Push only.** The viewer never contacts a prober, so a prober in a hostile network
  needs no exposed port.
- **One writer, under ~100 targets.** Scaling history out means moving the store behind
  the viewer, not running more SQLite writers.
- **OIDC and a Prometheus-compatible history backend** are not included: both need a real
  IdP / Prometheus to test against honestly.

### Known limitations

- History is a single SQLite file; two viewers on one volume corrupt it. The chart and the
  runbook both say so.
- An ephemeral prober that dies mid-queue loses the results it had buffered.
- The UI is dark-only and does not gzip its own responses.
