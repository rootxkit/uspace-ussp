package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Gap causes (writer_gaps.cause).
const (
	CauseStreamRemoved = "stream_removed"
	CauseMalformed     = "malformed"
	CauseRejected      = "rejected"
	// CausePositionUnknown is a consumer that acknowledged messages the
	// database records no position for (audit S3).
	CausePositionUnknown = "position_unknown"
)

// Count units of a gap.
const (
	UnitRows     = "rows"
	UnitMessages = "messages"
)

// Gap is one writer_gaps row.
type Gap struct {
	DedupeKey string
	Stream    string
	Subject   string
	FromSeq   uint64
	ToSeq     uint64
	Cause     string
	Count     int64
	CountUnit string
	// AfterAt and BeforeAt bound the hole in time: the captured_at of
	// the last message written before it and of the first after it,
	// when known.
	AfterAt, BeforeAt *time.Time
	Detail            string
}

// WriteBatch is one transaction: the rows per table, the gaps recorded
// beside them, and the stream position they bring the table to (0:
// none).
type WriteBatch struct {
	Stream   string
	Rows     map[*CopyTable][][]any
	Gaps     []Gap
	Position uint64
}

// Written is what a committed batch did.
type Written struct {
	Inserted   int64
	Duplicates int64
	Gaps       int64
}

// TSWriter writes batches into TimescaleDB as the writer's login role:
// each table's rows are copied (pgx.CopyFrom) into a session staging
// table and moved with one INSERT .. ON CONFLICT DO NOTHING on the
// (msg_id, time) unique index, so a redelivered message writes nothing
// twice and is counted as a duplicate (B-05). Gaps and the position
// commit in the same transaction; at is the database clock.
type TSWriter struct {
	Pool *Pool
}

// Write commits b in one transaction.
func (s TSWriter) Write(ctx context.Context, b WriteBatch) (Written, error) {
	var w Written
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return w, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for t, rows := range b.Rows {
		if len(rows) == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, t.DDL()); err != nil {
			return Written{}, fmt.Errorf("%s: %w", t.Stage(), err)
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{t.Stage()}, t.ColumnNames(), pgx.CopyFromRows(rows)); err != nil {
			return Written{}, fmt.Errorf("copy %s: %w", t.Name, err)
		}
		tag, err := tx.Exec(ctx, t.Insert)
		if err != nil {
			return Written{}, fmt.Errorf("insert %s: %w", t.Name, err)
		}
		w.Inserted += tag.RowsAffected()
		w.Duplicates += int64(len(rows)) - tag.RowsAffected()
	}
	for i := range b.Gaps {
		g := &b.Gaps[i]
		tag, err := tx.Exec(ctx, `INSERT INTO writer_gaps (dedupe_key, stream, subject, from_seq, to_seq, cause, count, count_unit,
	after_at, before_at, detail) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) ON CONFLICT (dedupe_key) DO NOTHING`,
			g.DedupeKey, g.Stream, g.Subject, int64(g.FromSeq), int64(g.ToSeq), g.Cause, g.Count, g.CountUnit, g.AfterAt, g.BeforeAt, g.Detail)
		if err != nil {
			return Written{}, fmt.Errorf("writer_gaps: %w", err)
		}
		w.Gaps += tag.RowsAffected()
	}
	if b.Position > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO writer_positions (stream, seq) VALUES ($1, $2)
ON CONFLICT (stream) DO UPDATE SET seq = GREATEST(writer_positions.seq, EXCLUDED.seq), at = now()`, b.Stream, int64(b.Position)); err != nil {
			return Written{}, fmt.Errorf("writer_positions: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Written{}, fmt.Errorf("commit: %w", err)
	}
	return w, nil
}

// Position is the highest stream sequence written for stream; false
// when none is recorded.
func (s TSWriter) Position(ctx context.Context, stream string) (uint64, bool, error) {
	var seq int64
	err := s.Pool.QueryRow(ctx, "SELECT seq FROM writer_positions WHERE stream = $1", stream).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return uint64(seq), true, nil
}

// IsDataError reports whether the database refused the data itself (a
// data exception, class 22, or an integrity violation, class 23), as
// opposed to being unreachable: such a batch is retried message by
// message and a message refused alone is recorded as rejected.
func IsDataError(err error) bool {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return false
	}
	return strings.HasPrefix(pe.Code, "22") || strings.HasPrefix(pe.Code, "23")
}

// CopyTable is one hypertable as the writer fills it: a staging table of
// plain types the rows are copied into (pgx.CopyFrom), and the INSERT
// that moves them into the hypertable with PostGIS and type casts, ON
// CONFLICT DO NOTHING on the (msg_id, time) unique index (B-05).
type CopyTable struct {
	Name string
	// Columns are the staging columns in row order, as name and type.
	Columns [][2]string
	// Insert is the statement from the staging table (named Stage()).
	Insert string
}

// Stage is the staging table's name.
func (t CopyTable) Stage() string { return "stage_" + t.Name }

// ColumnNames are the staging column names in row order.
func (t CopyTable) ColumnNames() []string {
	out := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		out[i] = c[0]
	}
	return out
}

// DDL creates the staging table for one session, emptied at every
// commit.
func (t CopyTable) DDL() string {
	cols := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		cols[i] = c[0] + " " + c[1]
	}
	return "CREATE TEMP TABLE IF NOT EXISTS " + t.Stage() + " (" + strings.Join(cols, ", ") + ") ON COMMIT DELETE ROWS"
}

var (
	// TableTelemetry is the own flights' track (track/telemetry/v1 with a
	// flight_id) from TRK.
	TableTelemetry = CopyTable{
		Name: "telemetry",
		Columns: [][2]string{
			{"msg_id", "text"}, {"flight_id", "text"}, {"captured_at", "timestamptz"}, {"ts", "timestamptz"}, {"rx_ts", "timestamptz"},
			{"backlog", "boolean"}, {"time_source", "text"}, {"lat_deg", "double precision"}, {"lon_deg", "double precision"},
			{"alt_wgs84_m", "double precision"}, {"alt_amsl_m", "double precision"}, {"alt_pressure_m", "double precision"},
			{"height_m", "double precision"}, {"height_ref", "text"}, {"speed_ms", "double precision"}, {"track_deg", "double precision"},
			{"vspeed_ms", "double precision"}, {"accuracy_h_m", "double precision"}, {"accuracy_v_m", "double precision"},
			{"status", "text"}, {"emergency", "boolean"}, {"source_client_id", "text"}, {"cell5", "text"},
		},
		Insert: `INSERT INTO telemetry (msg_id, flight_id, captured_at, ts, rx_ts, backlog, time_source, geom, alt_wgs84_m, alt_amsl_m,
	alt_pressure_m, height_m, height_ref, speed_ms, track_deg, vspeed_ms, accuracy_h_m, accuracy_v_m, status, emergency, source_client_id, cell5)
SELECT msg_id, flight_id::uuid, captured_at, ts, rx_ts, backlog, time_source, ST_SetSRID(ST_MakePoint(lon_deg, lat_deg), 4326),
	alt_wgs84_m, alt_amsl_m, alt_pressure_m, height_m, height_ref, speed_ms, track_deg, vspeed_ms, accuracy_h_m, accuracy_v_m,
	status, emergency, source_client_id, cell5
FROM stage_telemetry ON CONFLICT DO NOTHING`,
	}
	mannedColumns = [][2]string{
		{"msg_id", "text"}, {"icao24", "text"}, {"callsign", "text"}, {"captured_at", "timestamptz"}, {"ts", "timestamptz"},
		{"rx_ts", "timestamptz"}, {"lat_deg", "double precision"}, {"lon_deg", "double precision"}, {"alt_pressure_m", "double precision"},
		{"alt_wgs84_m", "double precision"}, {"gs_ms", "double precision"}, {"track_deg", "double precision"}, {"vrate_ms", "double precision"},
		{"emergency", "text"}, {"source_class", "text"}, {"quality", "text"}, {"adapter_id", "text"}, {"trust", "text"}, {"cell5", "text"},
	}
	// TableManned is manned traffic from the ANSP feed (MAN).
	TableManned = CopyTable{
		Name: "manned_tracks", Columns: mannedColumns,
		Insert: `INSERT INTO manned_tracks (msg_id, icao24, callsign, captured_at, ts, rx_ts, geom, alt_pressure_m, alt_wgs84_m, gs_ms,
	track_deg, vrate_ms, emergency, source_class, quality, adapter_id, trust, cell5)
SELECT msg_id, icao24, callsign, captured_at, ts, rx_ts, ST_SetSRID(ST_MakePoint(lon_deg, lat_deg), 4326), alt_pressure_m, alt_wgs84_m,
	gs_ms, track_deg, vrate_ms, emergency, source_class, quality, adapter_id, trust, cell5
FROM stage_manned_tracks ON CONFLICT DO NOTHING`,
	}
	// TableEconspicuity is e-conspicuity traffic from this USSP's own
	// receiver (MAN, source adsb_rx, trust broadcast).
	TableEconspicuity = CopyTable{
		Name: "econspicuity_tracks", Columns: append(append([][2]string{}, mannedColumns...), [2]string{"receiver_id", "text"}),
		Insert: `INSERT INTO econspicuity_tracks (msg_id, icao24, callsign, captured_at, ts, rx_ts, geom, alt_pressure_m, alt_wgs84_m, gs_ms,
	track_deg, vrate_ms, emergency, source_class, quality, adapter_id, trust, receiver_id, cell5)
SELECT msg_id, icao24, callsign, captured_at, ts, rx_ts, ST_SetSRID(ST_MakePoint(lon_deg, lat_deg), 4326), alt_pressure_m, alt_wgs84_m,
	gs_ms, track_deg, vrate_ms, emergency, source_class, quality, adapter_id, trust, receiver_id, cell5
FROM stage_econspicuity_tracks ON CONFLICT DO NOTHING`,
	}
	// TablePeerFlights are peer USSPs' flights (PEER), the state verbatim.
	TablePeerFlights = CopyTable{
		Name: "peer_flights",
		Columns: [][2]string{
			{"msg_id", "text"}, {"peer_uss", "text"}, {"rid_flight_id", "text"}, {"rx_ts", "timestamptz"}, {"state", "text"}, {"cell5", "text"},
		},
		Insert: `INSERT INTO peer_flights (msg_id, peer_uss, rid_flight_id, rx_ts, state, cell5)
SELECT msg_id, peer_uss, rid_flight_id, rx_ts, state::jsonb, cell5 FROM stage_peer_flights ON CONFLICT DO NOTHING`,
	}
	// TableTrafficProducts are the sampled products (TRAFFIC).
	TableTrafficProducts = CopyTable{
		Name: "traffic_products",
		Columns: [][2]string{
			{"msg_id", "text"}, {"client_id", "text"}, {"at", "timestamptz"}, {"intent_id", "text"},
			{"min_lat_deg", "double precision"}, {"min_lon_deg", "double precision"}, {"max_lat_deg", "double precision"}, {"max_lon_deg", "double precision"},
			{"tracks_shown", "text"}, {"degraded", "text[]"}, {"policy_version", "bigint"},
		},
		Insert: `INSERT INTO traffic_products (msg_id, client_id, at, intent_id, bbox, tracks_shown, degraded, policy_version)
SELECT msg_id, client_id, at, intent_id::uuid,
	CASE WHEN min_lat_deg IS NULL THEN NULL ELSE ST_MakeEnvelope(min_lon_deg, min_lat_deg, max_lon_deg, max_lat_deg, 4326) END,
	tracks_shown::jsonb, degraded, policy_version
FROM stage_traffic_products ON CONFLICT DO NOTHING`,
	}
	// TableConformanceSamples are the conformance outcomes (CONF).
	TableConformanceSamples = CopyTable{
		Name: "conformance_samples",
		Columns: [][2]string{
			{"msg_id", "text"}, {"flight_id", "text"}, {"captured_at", "timestamptz"}, {"state", "text"},
			{"distance_outside_m", "double precision"}, {"height_over_m", "double precision"},
		},
		Insert: `INSERT INTO conformance_samples (msg_id, flight_id, captured_at, state, distance_outside_m, height_over_m)
SELECT msg_id, flight_id::uuid, captured_at, state, distance_outside_m, height_over_m
FROM stage_conformance_samples ON CONFLICT DO NOTHING`,
	}
)
