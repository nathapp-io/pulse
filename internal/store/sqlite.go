package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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
  config_hash TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS targets (
  prober TEXT NOT NULL,
  name   TEXT NOT NULL,
  grp    TEXT NOT NULL DEFAULT '',
  typ    TEXT NOT NULL DEFAULT '',
  state  TEXT NOT NULL DEFAULT 'UNKNOWN',
  since  INTEGER NOT NULL DEFAULT 0,
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

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate sqlite: %w", err)
	}

	// Databases created before maintenance windows existed do not have the
	// column, and CREATE TABLE IF NOT EXISTS does not add it. SQLite has no
	// "ADD COLUMN IF NOT EXISTS", so the duplicate-column error is expected and
	// is the signal that the migration already ran.
	if _, err := db.Exec(`ALTER TABLE results ADD COLUMN maintenance INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		_ = db.Close()
		return nil, fmt.Errorf("migrate results.maintenance: %w", err)
	}
	return &sqliteStore{cfg: cfg, db: db}, nil
}

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

	for _, spec := range b.Targets {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO targets(prober, name, grp, typ, state, since) VALUES(?,?,?,?,'UNKNOWN',0)
			ON CONFLICT(prober, name) DO UPDATE SET grp=excluded.grp, typ=excluded.typ`,
			b.Prober.Name, spec.Name, spec.Group, spec.Type,
		); err != nil {
			return 0, fmt.Errorf("upsert target %q: %w", spec.Name, err)
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

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO targets(prober, name, state, since) VALUES(?,?,'UNKNOWN',0)
			ON CONFLICT(prober, name) DO NOTHING`, b.Prober.Name, r.Target,
		); err != nil {
			return 0, fmt.Errorf("ensure target %q: %w", r.Target, err)
		}
		state := model.NormalizeState(r.State)
		// `since <= ?` keeps an out-of-order (older) result from moving the
		// transition time backwards.
		if _, err := tx.ExecContext(ctx, `
			UPDATE targets SET state=?, since=? WHERE prober=? AND name=? AND state<>? AND since<=?`,
			state, r.At.UnixNano(), b.Prober.Name, r.Target, state, r.At.UnixNano(),
		); err != nil {
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
	var (
		snap     Snapshot
		probers  []ProberState
		targets  []TargetState
		targetOf = make(map[string]int) // target name -> index in targets
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
	rows, err = s.db.QueryContext(ctx, `SELECT prober, name, grp, typ, state, since FROM targets ORDER BY name`)
	if err != nil {
		return snap, fmt.Errorf("query targets: %w", err)
	}
	for rows.Next() {
		var (
			t     TargetState
			since int64
		)
		if err := rows.Scan(&t.Prober, &t.Spec.Name, &t.Spec.Group, &t.Spec.Type, &t.State, &since); err != nil {
			_ = rows.Close()
			return snap, err
		}
		t.Since = unixOrZero(since)
		t.Uptime = make(map[string]float64, len(UptimeWindows))
		targetOf[t.Spec.Name] = len(targets)
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return snap, err
	}
	_ = rows.Close()

	// Latest result per target (window function; SQLite >= 3.25).
	rows, err = s.db.QueryContext(ctx, `
		SELECT target, at, ok, state, duration_ms, status_code, error_class, error, tls_expiry, phase_json, maintenance
		FROM (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY target ORDER BY at DESC) AS rn
			FROM results
		) WHERE rn = 1`)
	if err != nil {
		return snap, fmt.Errorf("query last results: %w", err)
	}
	for rows.Next() {
		var (
			target    string
			r         model.Result
			at, tls   int64
			ok, maint int
			phases    string
		)
		if err := rows.Scan(&target, &at, &ok, &r.State, &r.DurationMS, &r.StatusCode,
			&r.ErrorClass, &r.Error, &tls, &phases, &maint); err != nil {
			_ = rows.Close()
			return snap, err
		}
		r.Target = target
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
		if idx, ok := targetOf[target]; ok {
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
	sortProbers(snap.Probers)
	sortTargets(snap.Targets)
	return snap, nil
}

func (s *sqliteStore) fillUptime(
	ctx context.Context,
	table string,
	window time.Duration,
	names []string,
	targets []TargetState,
	targetOf map[string]int,
) error {
	cutoff := time.Now().Add(-window).Unix()
	query := "SELECT target, SUM(ok), SUM(total) FROM " + table + " WHERE bucket >= ? GROUP BY target"

	rows, err := s.db.QueryContext(ctx, query, cutoff)
	if err != nil {
		return fmt.Errorf("query uptime from %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			target    string
			ok, total int64
		)
		if err := rows.Scan(&target, &ok, &total); err != nil {
			return err
		}
		idx, known := targetOf[target]
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

func (s *sqliteStore) fillLatency(ctx context.Context, targets []TargetState, targetOf map[string]int) error {
	cutoff := time.Now().Add(-LatencyWindow).UnixNano()
	rows, err := s.db.QueryContext(ctx,
		// ok = 1 only: a failed probe's duration says nothing about latency, and a
		// refused connection records 0 ms, which would read as "very fast".
		`SELECT target, duration_ms FROM results WHERE at >= ? AND ok = 1 ORDER BY target, at`, cutoff)
	if err != nil {
		return fmt.Errorf("query latency: %w", err)
	}
	defer func() { _ = rows.Close() }()

	buckets := make(map[string][]int64, len(targets))
	for rows.Next() {
		var (
			target   string
			duration int64
		)
		if err := rows.Scan(&target, &duration); err != nil {
			return err
		}
		buckets[target] = append(buckets[target], duration)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for target, durations := range buckets {
		if idx, ok := targetOf[target]; ok {
			targets[idx].Latency = Percentiles(durations)
		}
	}
	return nil
}

func (s *sqliteStore) History(ctx context.Context, target string, from time.Time) ([]model.Result, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT at, ok, state, duration_ms, status_code, error_class, error, tls_expiry, phase_json, maintenance
		FROM results WHERE target = ? AND at >= ? ORDER BY at`,
		target, from.UnixNano())
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

func unixOrZero(nanos int64) time.Time {
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos).UTC()
}
