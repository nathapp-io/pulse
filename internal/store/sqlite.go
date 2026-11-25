package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps the binary static and CGO-free

	"pulse/internal/model"
)

const schema = `
CREATE TABLE IF NOT EXISTS probers (
  name        TEXT PRIMARY KEY,
  version     TEXT NOT NULL DEFAULT '',
  labels      TEXT NOT NULL DEFAULT '{}',
  last_seen   INTEGER NOT NULL,
  config_hash TEXT NOT NULL DEFAULT '',
  declared    INTEGER NOT NULL DEFAULT 0,
  inventory_versioned INTEGER NOT NULL DEFAULT 0,
  inventory_order INTEGER
);

CREATE TABLE IF NOT EXISTS targets (
  prober TEXT NOT NULL,
  name   TEXT NOT NULL,
  grp    TEXT NOT NULL DEFAULT '',
  typ    TEXT NOT NULL DEFAULT '',
  state  TEXT NOT NULL DEFAULT 'UNKNOWN',
  since  INTEGER NOT NULL DEFAULT 0,
  active INTEGER NOT NULL DEFAULT 1,
  latest_at INTEGER,                     -- newest accepted probe time, independent of retention
  before_run INTEGER,                    -- last differing state before the current run
  PRIMARY KEY (prober, name)
);

CREATE TABLE IF NOT EXISTS results (
  prober      TEXT NOT NULL,
  target      TEXT NOT NULL,
  at          INTEGER NOT NULL,          -- unix nanoseconds
  ok          INTEGER NOT NULL,
  state       TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL DEFAULT 0,
  status_code INTEGER NOT NULL DEFAULT 0,
  error_class TEXT NOT NULL DEFAULT '',
  error       TEXT NOT NULL DEFAULT '',
  tls_expiry  INTEGER NOT NULL DEFAULT 0,
  phase_json  TEXT NOT NULL DEFAULT '{}',
  maintenance INTEGER NOT NULL DEFAULT 0, -- taken inside a maintenance window
  PRIMARY KEY (prober, target, at)       -- makes ingest idempotent
);

CREATE INDEX IF NOT EXISTS idx_results_target_at ON results(target, at);
CREATE INDEX IF NOT EXISTS idx_results_at ON results(at);

CREATE TABLE IF NOT EXISTS rollup_1m (
  prober          TEXT NOT NULL,
  target          TEXT NOT NULL,
  bucket          INTEGER NOT NULL,      -- unix seconds, truncated to the minute
  total           INTEGER NOT NULL,
  ok              INTEGER NOT NULL,
  sum_duration_ms INTEGER NOT NULL,
  PRIMARY KEY (prober, target, bucket)
);

CREATE TABLE IF NOT EXISTS rollup_1h (
  prober          TEXT NOT NULL,
  target          TEXT NOT NULL,
  bucket          INTEGER NOT NULL,      -- unix seconds, truncated to the hour
  total           INTEGER NOT NULL,
  ok              INTEGER NOT NULL,
  sum_duration_ms INTEGER NOT NULL,
  PRIMARY KEY (prober, target, bucket)
);
`

type sqliteStore struct {
	cfg Config
	db  *sql.DB

	mu        sync.Mutex // one writer at a time; reads are serialised by MaxOpenConns
	lastPrune time.Time
}

func openSQLite(cfg Config) (*sqliteStore, error) {
	dsn := "file:" + cfg.Path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Under 100 targets the write volume is ~3 results/second; one connection
	// removes SQLITE_BUSY entirely and keeps the code single-writer.
	db.SetMaxOpenConns(1)

	if err := migrateSQLite(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate sqlite: %w", err)
	}
	return &sqliteStore{cfg: cfg, db: db}, nil
}

// Older databases had no declaration marker: preserve their visible targets
// until the prober next sends an authoritative list. Upgrade and backfill in
// one transaction so an interrupted migration can be retried safely.
func migrateSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	for _, c := range []struct{ table, name, definition string }{
		{"results", "maintenance", "INTEGER NOT NULL DEFAULT 0"},
		{"probers", "declared", "INTEGER NOT NULL DEFAULT 0"},
		{"targets", "active", "INTEGER NOT NULL DEFAULT 1"},
		{"targets", "latest_at", "INTEGER"},
		{"targets", "before_run", "INTEGER"},
		{"probers", "inventory_versioned", "INTEGER NOT NULL DEFAULT 0"},
		{"probers", "inventory_order", "INTEGER"},
	} {
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name=?)`, c.table, c.name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(`ALTER TABLE ` + c.table + ` ADD COLUMN ` + c.name + ` ` + c.definition); err != nil {
			return err
		}
		var backfill string
		switch c.name {
		case "latest_at":
			backfill = backfillLatest
		case "before_run":
			backfill = backfillRun
		case "inventory_order":
			// Old rows didn't distinguish results with/without a declaration.
			// Use the high-water mark across ALL targets (even inactive/pruned)
			// as the best available replay fence for previously declared lists.
			backfill = `UPDATE probers SET inventory_order=(SELECT MAX(latest_at) FROM targets WHERE prober=probers.name) WHERE declared=1`
		}
		if backfill != "" {
			if _, err := tx.Exec(backfill); err != nil {
				return fmt.Errorf("backfill %s: %w", c.name, err)
			}
		}
	}
	return tx.Commit()
}

// A legacy state's since timestamp is not a last-sample watermark: a
// delayed transition may even have overwritten the state. Use the latest
// retained sample and the start of its run. Preserve an earlier transition
// timestamp if the run started before raw retention, and fall back to since
// as the best known watermark when all raw samples have expired.
const backfillLatest = `
UPDATE targets AS t SET latest_at =
  (SELECT MAX(at) FROM results WHERE prober=t.prober AND target=t.name);
UPDATE targets SET latest_at=since WHERE since<>0 AND (latest_at IS NULL OR since>latest_at);
UPDATE targets AS t SET state=r.state, since=COALESCE(
  (SELECT MIN(at) FROM results WHERE prober=t.prober AND target=t.name AND at >
    (SELECT MAX(at) FROM results WHERE prober=t.prober AND target=t.name AND state<>r.state)),
  (SELECT MIN(at) FROM results WHERE prober=t.prober AND target=t.name))
FROM results AS r WHERE r.prober=t.prober AND r.target=t.name AND r.at=t.latest_at AND r.state<>t.state;`

// The branch schema already has latest_at, but its since could miss late
// observations. Reconcile the old summary with surviving evidence only once.
const backfillRun = `
UPDATE targets AS t SET before_run=(SELECT MAX(at) FROM results WHERE prober=t.prober AND target=t.name AND at<t.latest_at AND state<>t.state);
UPDATE targets AS t SET since=MIN(
  CASE WHEN before_run IS NULL OR since>before_run THEN since ELSE latest_at END,
  COALESCE((SELECT MIN(at) FROM results WHERE prober=t.prober AND target=t.name AND state=t.state AND (t.before_run IS NULL OR at>t.before_run)),latest_at))
WHERE latest_at IS NOT NULL;`

func (s *sqliteStore) Ingest(ctx context.Context, b model.Batch, receivedAt time.Time) (int, error) {
	if err := b.Validate(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	labels, err := json.Marshal(b.Prober.Labels)
	if err != nil {
		return 0, fmt.Errorf("encode labels: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO probers(name, version, labels, last_seen, config_hash) VALUES(?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET
			version=excluded.version, labels=excluded.labels,
			last_seen=excluded.last_seen, config_hash=excluded.config_hash`,
		b.Prober.Name, b.Prober.Version, string(labels), receivedAt.UnixNano(), b.ConfigHash,
	); err != nil {
		return 0, fmt.Errorf("upsert prober: %w", err)
	}

	if b.Targets != nil {
		if err := reconcileSQLite(ctx, tx, b); err != nil {
			return 0, err
		}
	}

	stored := 0
	for _, r := range b.Results {
		phase, err := json.Marshal(r.PhaseMS)
		if err != nil {
			return 0, fmt.Errorf("encode phases for %s: %w", r.Target, err)
		}
		var tlsExpiry int64
		if r.TLSExpiry != nil {
			tlsExpiry = r.TLSExpiry.UnixNano()
		}

		res, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO results(prober, target, at, ok, state, duration_ms,
				status_code, error_class, error, tls_expiry, phase_json, maintenance)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			b.Prober.Name, r.Target, r.At.UnixNano(), boolToInt(r.OK), model.NormalizeState(r.State),
			r.DurationMS, r.StatusCode, r.ErrorClass, r.Error, tlsExpiry, string(phase),
			boolToInt(r.Maintenance),
		)
		if err != nil {
			return 0, fmt.Errorf("insert result %s@%s: %w", r.Target, r.At.Format(time.RFC3339Nano), err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if affected == 0 {
			continue // already stored: a retried push
		}
		stored++

		for _, rollup := range []struct {
			table  string
			bucket int64
		}{
			{"rollup_1m", r.At.Truncate(time.Minute).Unix()},
			{"rollup_1h", r.At.Truncate(time.Hour).Unix()},
		} {
			query := `INSERT INTO ` + rollup.table + `(prober, target, bucket, total, ok, sum_duration_ms)
				VALUES(?,?,?,1,?,?)
				ON CONFLICT(prober, target, bucket) DO UPDATE SET
					total=total+1, ok=ok+excluded.ok,
					sum_duration_ms=sum_duration_ms+excluded.sum_duration_ms`
			if _, err := tx.ExecContext(ctx, query,
				b.Prober.Name, r.Target, rollup.bucket, boolToInt(r.OK), r.DurationMS,
			); err != nil {
				return 0, fmt.Errorf("upsert %s: %w", rollup.table, err)
			}
		}

		if err := recordSQLite(ctx, tx, b.Prober.Name, r); err != nil {
			return 0, fmt.Errorf("update state for %q: %w", r.Target, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	if err := s.pruneLocked(ctx, receivedAt); err != nil {
		return stored, err
	}
	return stored, nil
}

func reconcileSQLite(ctx context.Context, tx *sql.Tx, b model.Batch) error {
	order := declarationOrder(b)
	accepted, err := tx.ExecContext(ctx, `UPDATE probers SET declared=1, inventory_versioned=?, inventory_order=?
		WHERE name=? AND (inventory_order IS NULL OR (?,?) > (inventory_versioned,inventory_order))`,
		order.versioned, order.value, b.Prober.Name, order.versioned, order.value)
	if err != nil {
		return fmt.Errorf("order inventory: %w", err)
	}
	n, err := accepted.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE targets SET active=0 WHERE prober=?`, b.Prober.Name); err != nil {
		return fmt.Errorf("reconcile active targets: %w", err)
	}
	for _, spec := range b.Targets {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO targets(prober, name, grp, typ, state, since) VALUES(?,?,?,?,'UNKNOWN',0)
			ON CONFLICT(prober, name) DO UPDATE SET grp=excluded.grp, typ=excluded.typ, active=1`,
			b.Prober.Name, spec.Name, spec.Group, spec.Type,
		); err != nil {
			return fmt.Errorf("upsert target %q: %w", spec.Name, err)
		}
	}
	return nil
}

// Read just the summary on ordinary writes. A late transition inside the
// current run needs one additional successor seek on the results primary key.
func recordSQLite(ctx context.Context, tx *sql.Tx, prober string, r model.Result) error {
	var run stateRun
	var latest, boundary sql.NullInt64
	var since int64
	err := tx.QueryRowContext(ctx, `SELECT state, since, latest_at, before_run FROM targets WHERE prober=? AND name=?`, prober, r.Target).Scan(&run.state, &since, &latest, &boundary)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if latest.Valid {
		run.latest, run.since = time.Unix(0, latest.Int64), time.Unix(0, since)
	}
	if boundary.Valid {
		run.beforeRun = time.Unix(0, boundary.Int64)
	}
	state := model.NormalizeState(r.State)
	var next time.Time
	if state != run.state && r.At.After(run.since) && r.At.Before(run.latest) {
		var at sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MIN(at) FROM results WHERE prober=? AND target=? AND at>?`, prober, r.Target, r.At.UnixNano()).Scan(&at); err != nil {
			return err
		}
		if at.Valid {
			next = time.Unix(0, at.Int64)
		}
	}
	previous := run
	run.record(state, r.At, next)
	if run == previous {
		return nil
	}
	var before any
	if !run.beforeRun.IsZero() {
		before = run.beforeRun.UnixNano()
	}
	// Results never change an existing target's membership. For a new name,
	// infer membership only if the prober has never supplied a target list.
	_, err = tx.ExecContext(ctx, `INSERT INTO targets(prober,name,state,since,latest_at,before_run,active)
		SELECT name,?,?,?,?,?, NOT declared FROM probers WHERE name=?
		ON CONFLICT(prober,name) DO UPDATE SET state=excluded.state, since=excluded.since,
			latest_at=excluded.latest_at, before_run=excluded.before_run`,
		r.Target, run.state, run.since.UnixNano(), run.latest.UnixNano(), before, prober)
	return err
}

// pruneLocked drops expired rows at most once a minute.
func (s *sqliteStore) pruneLocked(ctx context.Context, now time.Time) error {
	if !s.lastPrune.IsZero() && now.Sub(s.lastPrune) < time.Minute {
		return nil
	}
	s.lastPrune = now

	for _, job := range []struct {
		table  string
		column string
		cutoff int64
	}{
		{"results", "at", now.Add(-s.cfg.Retention).UnixNano()},
		{"rollup_1m", "bucket", now.Add(-s.cfg.Retention1M).Unix()},
		{"rollup_1h", "bucket", now.Add(-s.cfg.Retention1H).Unix()},
	} {
		query := "DELETE FROM " + job.table + " WHERE " + job.column + " < ?"
		if _, err := s.db.ExecContext(ctx, query, job.cutoff); err != nil {
			return fmt.Errorf("prune %s: %w", job.table, err)
		}
	}
	return nil
}

func (s *sqliteStore) Snapshot(ctx context.Context) (Snapshot, error) {
	s.mu.Lock() // keep inventory, counts and samples from the same ingest
	defer s.mu.Unlock()
	var (
		snap     Snapshot
		probers  []ProberState
		targets  []TargetState
		targetOf = make(map[targetKey]int)
	)

	// Probers.
	rows, err := s.db.QueryContext(ctx, `SELECT name, version, labels, last_seen FROM probers ORDER BY name`)
	if err != nil {
		return snap, fmt.Errorf("query probers: %w", err)
	}
	for rows.Next() {
		var (
			p      ProberState
			labels string
			seen   int64
		)
		if err := rows.Scan(&p.Info.Name, &p.Info.Version, &labels, &seen); err != nil {
			_ = rows.Close()
			return snap, err
		}
		if labels != "" {
			_ = json.Unmarshal([]byte(labels), &p.Info.Labels)
		}
		p.LastSeen = time.Unix(0, seen).UTC()
		probers = append(probers, p)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return snap, err
	}
	_ = rows.Close()

	// Targets.
	rows, err = s.db.QueryContext(ctx, `SELECT prober, name, grp, typ, state, CASE WHEN latest_at IS NOT NULL THEN since END FROM targets WHERE active=1`)
	if err != nil {
		return snap, fmt.Errorf("query targets: %w", err)
	}
	for rows.Next() {
		var (
			t     TargetState
			since sql.NullInt64
		)
		if err := rows.Scan(&t.Prober, &t.Spec.Name, &t.Spec.Group, &t.Spec.Type, &t.State, &since); err != nil {
			_ = rows.Close()
			return snap, err
		}
		if since.Valid {
			t.Since = time.Unix(0, since.Int64).UTC()
		}
		t.Uptime = make(map[string]float64, len(UptimeWindows))
		targetOf[targetKey{t.Prober, t.Spec.Name}] = len(targets)
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return snap, err
	}
	_ = rows.Close()

	// Join on the watermark, not the newest surviving raw row: an older
	// backlog sample may arrive after the latest result has been pruned.
	rows, err = s.db.QueryContext(ctx, `
		SELECT r.prober, target, at, ok, r.state, duration_ms, status_code, error_class, error, tls_expiry, phase_json, maintenance
		FROM targets AS t JOIN results AS r ON r.prober=t.prober AND r.target=t.name AND r.at=t.latest_at
		WHERE t.active=1`)
	if err != nil {
		return snap, fmt.Errorf("query last results: %w", err)
	}
	for rows.Next() {
		var (
			key       targetKey
			r         model.Result
			at, tls   int64
			ok, maint int
			phases    string
		)
		if err := rows.Scan(&key.prober, &key.name, &at, &ok, &r.State, &r.DurationMS, &r.StatusCode,
			&r.ErrorClass, &r.Error, &tls, &phases, &maint); err != nil {
			_ = rows.Close()
			return snap, err
		}
		r.Target = key.name
		r.At = time.Unix(0, at).UTC()
		r.OK = ok == 1
		if tls > 0 {
			v := time.Unix(0, tls).UTC()
			r.TLSExpiry = &v
		}
		if phases != "" && phases != "null" {
			_ = json.Unmarshal([]byte(phases), &r.PhaseMS)
		}
		r.Maintenance = maint == 1
		if idx, ok := targetOf[key]; ok {
			targets[idx].Last = r
			targets[idx].Maintenance = r.Maintenance
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return snap, err
	}
	_ = rows.Close()

	// Uptime: minutes for 1h, hours for 24h and 7d.
	if err := s.fillUptime(ctx, "rollup_1m", time.Hour, []string{"1h"}, targets, targetOf); err != nil {
		return snap, err
	}
	if err := s.fillUptime(ctx, "rollup_1h", 24*time.Hour, []string{"24h"}, targets, targetOf); err != nil {
		return snap, err
	}
	if err := s.fillUptime(ctx, "rollup_1h", 7*24*time.Hour, []string{"7d"}, targets, targetOf); err != nil {
		return snap, err
	}

	// Latency percentiles from raw results inside the latency window.
	if err := s.fillLatency(ctx, targets, targetOf); err != nil {
		return snap, err
	}

	snap.Probers = probers
	snap.Targets = targets
	snap.finish()
	return snap, nil
}

func (s *sqliteStore) fillUptime(
	ctx context.Context,
	table string,
	window time.Duration,
	names []string,
	targets []TargetState,
	targetOf map[targetKey]int,
) error {
	cutoff := time.Now().Add(-window).Unix()
	query := "SELECT prober,target, SUM(ok), SUM(total) FROM " + table + " WHERE bucket >= ? GROUP BY prober,target"

	rows, err := s.db.QueryContext(ctx, query, cutoff)
	if err != nil {
		return fmt.Errorf("query uptime from %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			key       targetKey
			ok, total int64
		)
		if err := rows.Scan(&key.prober, &key.name, &ok, &total); err != nil {
			return err
		}
		idx, known := targetOf[key]
		if !known {
			continue
		}
		ratio := Ratio(ok, total)
		if ratio < 0 {
			continue
		}
		for _, name := range names {
			targets[idx].Uptime[name] = ratio
		}
	}
	return rows.Err()
}

func (s *sqliteStore) fillLatency(ctx context.Context, targets []TargetState, targetOf map[targetKey]int) error {
	cutoff := time.Now().Add(-LatencyWindow).UnixNano()
	rows, err := s.db.QueryContext(ctx,
		// ok = 1 only: a failed probe's duration says nothing about latency, and a
		// refused connection records 0 ms, which would read as "very fast".
		`SELECT prober,target, duration_ms FROM results WHERE at >= ? AND ok = 1`, cutoff)
	if err != nil {
		return fmt.Errorf("query latency: %w", err)
	}
	defer func() { _ = rows.Close() }()

	buckets := make(map[targetKey][]int64, len(targets))
	for rows.Next() {
		var (
			key      targetKey
			duration int64
		)
		if err := rows.Scan(&key.prober, &key.name, &duration); err != nil {
			return err
		}
		buckets[key] = append(buckets[key], duration)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for key, durations := range buckets {
		if idx, ok := targetOf[key]; ok {
			targets[idx].Latency = Percentiles(durations)
		}
	}
	return nil
}

func (s *sqliteStore) History(ctx context.Context, prober, target string, from time.Time) ([]model.Result, error) {
	if prober == "" {
		var owners int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(prober),'') FROM targets WHERE name=?`, target).Scan(&owners, &prober); err != nil {
			return nil, err
		}
		if owners > 1 {
			return nil, ErrAmbiguousTarget
		}
		if owners == 0 {
			return nil, nil
		}
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT at, ok, state, duration_ms, status_code, error_class, error, tls_expiry, phase_json, maintenance
		FROM results WHERE prober=? AND target = ? AND at >= ? ORDER BY at`,
		prober, target, from.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.Result
	for rows.Next() {
		var (
			r         model.Result
			at, tls   int64
			ok, maint int
			phases    string
		)
		if err := rows.Scan(&at, &ok, &r.State, &r.DurationMS, &r.StatusCode,
			&r.ErrorClass, &r.Error, &tls, &phases, &maint); err != nil {
			return nil, err
		}
		r.Target = target
		r.At = time.Unix(0, at).UTC()
		r.OK = ok == 1
		r.Maintenance = maint == 1
		if tls > 0 {
			v := time.Unix(0, tls).UTC()
			r.TLSExpiry = &v
		}
		if phases != "" && phases != "null" {
			_ = json.Unmarshal([]byte(phases), &r.PhaseMS)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *sqliteStore) Close() error { return s.db.Close() }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
