# Runbook

Operational procedures for a pulse deployment. Commands assume the Helm release is
named `pulse` in namespace `monitoring`; adjust for your install.

## 1. What runs where

| Component | Listens | Reachable from | Holds |
|---|---|---|---|
| `pulse-viewer` | `:8080` (UI, API, `/healthz`, `/metrics`) | probers + your ingress only | **all history** |
| `pulse-prober` | `:8081` (`/metrics`, `/healthz`) | nothing — no Service, no inbound rule | current state + push queue |

The prober only dials out. If you ever see a Service or an inbound NetworkPolicy rule
for it, something is wrong: nothing needs to reach a prober.

## 2. Deploy

```sh
helm upgrade --install pulse deploy/helm/pulse -n monitoring --create-namespace \
  -f my-values.yaml
```

Images: CI publishes `ghcr.io/nathapp-io/pulse/viewer` and `.../prober` on every push
to `main` (`latest`) and on every release tag, using the run's own `GITHUB_TOKEN` (the job
declares `packages: write`) — no personal token and no repository secret is involved.

The packages are **public**, so no pull secret is needed and the chart defaults work as-is.
Verified anonymously: both manifests list `linux/amd64` and `linux/arm64`, and a compose
stack started from these images reports its targets `UP`.

```sh
# confirms it is pullable without credentials, and shows both architectures
TOKEN=$(curl -s "https://ghcr.io/token?scope=repository:nathapp-io/pulse/prober:pull&service=ghcr.io" \
  | jq -r .token)
curl -s -H "Authorization: Bearer $TOKEN" \
  -H 'Accept: application/vnd.oci.image.index.v1+json' \
  https://ghcr.io/v2/nathapp-io/pulse/prober/manifests/latest | jq '[.manifests[].platform]'
```

For a **fork** publishing under its own namespace: a new ghcr package is created private
even when the repository is public, and the workflow token can push but cannot change
visibility. Flip it once (`gh auth refresh -s write:packages`, then
`gh api --method PATCH /orgs/<org>/packages/container/pulse%2Fprober -f visibility=public`),
or set the org's default package visibility to public for future packages.
`imagePullSecrets` is only for private mirrors and air-gapped installs.

Pin a version rather than tracking `latest` in production.

If an `images` job fails with `buildx failed with:` followed by a blob of HTML, that is a
transient registry error from ghcr, not a build problem: re-run the failed job.

Set before the first deploy, or you will get an error by design:

- `viewer.auth.password` (or `viewer.auth.existingSecret`)
- `prober.probes` — empty means a prober with nothing to probe

The chart refuses to render when `viewer.replicaCount > 1` with `store: sqlite`
(two writers corrupt history) and when persistence is enabled with `store: memory`
(nothing would be written to it).

**First-run checklist**

```sh
kubectl -n monitoring rollout status deploy/pulse-pulse-viewer
kubectl -n monitoring rollout status deploy/pulse-pulse-prober
kubectl -n monitoring logs deploy/pulse-pulse-viewer  | grep 'viewer started'   # store=sqlite db=/var/lib/pulse/pulse.db
kubectl -n monitoring logs deploy/pulse-pulse-prober  | grep 'pushing results'
curl -u admin:"$PASS" https://pulse.example.com/api/v1/status | jq .summary
```

`summary.probers_missing` must be 0. If it is 1, the viewer is up but no prober has
ever pushed — check the prober's token and the viewer's `PULSE_EXPECTED_PROBERS`.

## 3. Reading the state

| State | Meaning | Action |
|---|---|---|
| `UP` | last probe succeeded | none |
| `DOWN` | failed, past its `confirm_down` threshold | see the error class |
| `STALE` | **the prober is silent** — not necessarily the target | check the prober first |
| `MISSING` | declared in `expected_probers`, never reported | check the prober's deployment and token |
| `UNKNOWN` | configured, no result yet | wait one interval |
| `MAINTENANCE` | result inside a declared window; still recorded, not alarmed | none |

Error classes: `DNS_FAIL`, `CONNECT_REFUSED`, `CONNECT_TIMEOUT`, `TLS_HANDSHAKE`,
`TLS_EXPIRED`, `TLS_EXPIRING`, `HTTP_TIMEOUT`, `HTTP_STATUS`, `ASSERT_FAIL`,
`BODY_TOO_LARGE`, `ERROR`.

`ASSERT_FAIL` on a `200` is the most common false alarm and the most common real
outage at the same time: the endpoint is up and answering the wrong thing. The
message names the assertion, e.g. `body does not match /"status":"ok"/`.

## 4. Everything STALE at once

The path from prober to viewer is broken, not the targets.

```sh
# On the prober's /metrics:
#   pulse_push_failures_total climbing  -> the viewer is rejecting or unreachable
#   pulse_push_queued growing           -> results are piling up (they are kept, up to queue_size)
#   pulse_push_dropped_total > 0        -> the queue overflowed; history now has a gap
kubectl -n monitoring logs deploy/pulse-pulse-prober | grep -i 'push\|401'
kubectl -n monitoring logs deploy/pulse-pulse-viewer | grep 'ingest rejected'
```

Causes in order of likelihood: viewer not ready; wrong `viewer.url`; token rotated but
the prober's Secret not updated; NetworkPolicy blocking the prober.

Nothing is lost while the queue holds: results stay queued and are retried with
backoff, and ingest is idempotent, so a recovery replays cleanly. `dropped_total` above
zero is the only case where history has a hole.

## 5. Rotate or revoke an ingest token

Choose **one viewer source**. The viewer checks `PULSE_INGEST_TOKENS_FILE` first,
then `PULSE_INGEST_TOKENS`, then the shared `PULSE_INGEST_TOKEN`. It does not
merge sources or use a shared token as a fallback for a missing binding.

| Mode | Viewer | Chart prober |
|---|---|---|
| Shared (default) | `PULSE_INGEST_TOKEN` from `viewer.ingest.existingSecret`, or a managed Secret using `viewer.ingest.token` (random if empty) | Same Secret, same key |
| Bound env | `viewer.ingest.tokens` as `PULSE_INGEST_TOKENS` (comma-separated `name=token` pairs) | Set `viewer.ingest.existingSecret` to a Secret containing only this sender's `PULSE_INGEST_TOKEN` |
| Bound file | `viewer.ingest.existingTokensSecret` projected as a file (`tokensSecretKey`, default `ingest-tokens`; one `name=token` per line) | Same sender setup as bound env; the prober does not mount the viewer's file |

For example, provision the two file-mode Secrets in `monitoring` before the upgrade,
then set:

```yaml
viewer:
  ingest:
    existingTokensSecret: pulse-receiver  # key: ingest-tokens
    existingSecret: pulse-sender          # key: PULSE_INGEST_TOKEN
prober:
  name: core
```

The sender's token must match the `core` entry. For a viewer with only remote
probers, set `prober.replicaCount: 0`; no chart sender Secret is needed.
Set `viewer.expectedProbers` to the remote names that must report. With zero
bundled replicas and no explicit names, the viewer expects no probers by default.
`prober.tokenEnv` changes the **destination** env name, not the Secret key.

To revoke without restarting the viewer, remove the entry from the file Secret.
Allow for Kubernetes to update the projected volume; once updated, the viewer
reloads on use with a 2 s cache. An empty file revokes all tokens on reload. A
missing or malformed file keeps the **last good file set** and logs a warning;
an unreadable, malformed or empty file at startup prevents the viewer starting.
To rotate rather than revoke, also update the matching sender credential. For
chart pods that read ingest credentials from env, increment `viewer.ingest.revision`
in your saved Helm values and apply the upgrade. Update external Secrets **before**
the upgrade; change managed token values in the same upgrade. The counter rolls
the viewer in shared and bound-env modes, and the bundled prober when it uses a
token. A file-mode viewer does not restart. Restart remote env-based senders
separately after updating their credentials.

## 6. Rotate the viewer password

```sh
# bcrypt is accepted and preferred over plaintext:
htpasswd -bnBC 12 "" 'new-password' | tr -d ':\n'
```

Set `viewer.auth.passwordHash` in your private Helm values, or update
`viewer.auth.existingSecret` first (supply exactly one of `PULSE_AUTH_PASSWORD`
and `PULSE_AUTH_PASSWORD_HASH`). Increment `viewer.auth.revision` in your saved
values and apply the upgrade. Only the viewer restarts; the prober never reads
the password. Browsers may also need a hard reload.

Both credential revisions are operator-managed counters (e.g. `0` → `1`), not
Secret contents, hashes, or Helm release numbers. Keep them unchanged on unrelated
upgrades. Changing env credential data under the same Secret name and key does
not by itself restart a pod; bump the corresponding counter on rotation, whether
you use live Helm or an offline renderer.

### Credential checklist before upgrading from 0.1.0

- **Default managed credentials:** keep the same release name and namespace and
  reapply your auth values. Auth keys move to `<fullname>-auth`. The managed
  ingest Secret keeps the old `<fullname>-viewer` name: a live Helm upgrade
  uses `lookup` to reuse its `PULSE_INGEST_TOKEN` rather than rotate it. An
  offline `helm template` cannot do that lookup. For GitOps/offline rendering,
  provision a stable external Secret first instead of relying on generation.
- **Old combined or custom-named Secret:** check the old release manifest
  privately before upgrading (it contains credentials). Older charts could
  include `viewer.auth.existingSecret` as a Helm-managed resource; simply
  switching it to reference-only can cause Helm to **delete** it on upgrade.
  Before upgrading, securely copy the needed keys (including the old generated
  `PULSE_INGEST_TOKEN` if keeping shared mode) to a **new**, separately
  managed Secret. Set both `viewer.auth.existingSecret` and
  `viewer.ingest.existingSecret` to the new name, or split the keys between
  two Secrets. Do not point either value at a retiring Helm-owned Secret.
  A genuinely external combined Secret can be referenced by both settings.
- **Bound/file installs:** add every intended name to the authoritative list,
  including the chart prober. Provision its matching sender Secret as in §5;
  it no longer inherits all keys from the viewer Secret. If using a custom
  `prober.tokenEnv`, ensure the source key is named `PULSE_INGEST_TOKEN`.
  Remove stale static credentials; they will not be used as a fallback.
- Use a viewer image containing the file-precedence fix. After upgrade, verify
  both rollouts and that each expected prober checks in (§2).

## 7. Disk growth and retention

Raw rows are ~100–150 B. At 100 targets and a 30 s interval: **~30–40 MB/day**, so the
default `retention.raw: 168h` (7d) is ~250 MB; rollups are negligible.

```sh
kubectl -n monitoring exec deploy/pulse-pulse-viewer -- ls -l /var/lib/pulse   # no shell in the image: use a debug pod instead
kubectl -n monitoring get pvc pulse-pulse-viewer
```

The viewer prunes at most once a minute: raw results older than `PULSE_RETENTION_RAW`,
`rollup_1m` older than `PULSE_RETENTION_1M`, `rollup_1h` older than `PULSE_RETENTION_1H`.
Shorten retention before resizing the volume; halving the interval doubles the disk.

## 8. Backup and restore

The database is a single SQLite file (WAL enabled). For a consistent copy, stop the
writer:

```sh
kubectl -n monitoring scale deploy/pulse-pulse-viewer --replicas=0
kubectl -n monitoring exec deploy/… # or copy from the PVC with a helper pod
kubectl -n monitoring scale deploy/pulse-pulse-viewer --replicas=1
```

Restore is the reverse: put the file at `PULSE_DB` before the viewer starts. Cosmic
rule: **one writer**. Never run two viewers on the same file, not even briefly.

## 9. Upgrades and migrations

Migrations run at startup and are idempotent (`results.maintenance` is added with an
`ALTER TABLE` that tolerates already-migrated databases). The viewer Deployment uses
`strategy: Recreate` because the volume is `ReadWriteOnce`: expect a few seconds of
downtime per upgrade, during which probers queue their results.

`helm uninstall` keeps the PVC (`viewer.persistence.keepOnDelete: true`) so history
survives a reinstall. Delete it explicitly when you mean it.

## 10. Limits to design around

- **Under ~100 targets** per viewer. One writer, batched inserts, simple retention.
  Beyond that, revisit storage before adding replicas.
- **One viewer per history.** Scaling out means the store moves behind it (Postgres or
  a Prometheus-compatible backend), not that you run more SQLite writers.
- Probers are cheap and disposable: run one per network that needs a vantage point.
  Each needs its own token and name.

## 11. What to alert on

| Signal | Why |
|---|---|
| `pulse_push_dropped_total > 0` | history has a hole; the queue overflowed |
| `pulse_push_failures_total` rising for >5 min | prober cannot reach the viewer |
| `pulse_push_queued` growing monotonically | same, earlier warning |
| `pulse_push_last_success_timestamp_seconds` stale | pushes stopped entirely |
| viewer `/healthz` failing | the only exposed component is down |
| `summary.probers_missing > 0` | a declared prober never reported |
