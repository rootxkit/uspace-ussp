//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/f3411"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
	"github.com/rootxkit/uspace-ussp/internal/store/timeseries"
)

// The databases of the integration suite:
//
//	USSP_TEST_PG_URL        relational, as ussp_api (the owner: migrates);
//	                        the application pools SET ROLE ussp_app
//	USSP_TEST_TS_URL        time series, as ussp_api (read-only)
//	USSP_TEST_TS_OWNER_URL  time series, as ussp_tsdb (the owner: migrates, writes)

func pool(t *testing.T, url, role string) *store.Pool {
	t.Helper()
	p, err := store.OpenPool(store.PoolOptions{URL: url, MaxConns: 4, Role: role, ApplicationName: "ussp-integration"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func relOwner(t *testing.T) *store.Pool { return pool(t, mustEnv(t, "USSP_TEST_PG_URL"), "") }
func relApp(t *testing.T) *store.Pool   { return pool(t, mustEnv(t, "USSP_TEST_PG_URL"), store.AppRole) }
func tsOwner(t *testing.T) *store.Pool  { return pool(t, mustEnv(t, "USSP_TEST_TS_OWNER_URL"), "") }

// appStore is the relational store as the api process opens it.
func appStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Config{RelURL: mustEnv(t, "USSP_TEST_PG_URL"), RelRole: store.AppRole, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

// ensureSchemas brings both test databases to the latest version, as
// the migrate subcommands would; every test that needs a schema calls it
// (the suite passes in any order).
func ensureSchemas(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := relOwner(t).Migrate(ctx, store.TreeRelational); err != nil {
		t.Fatal(err)
	}
	if _, err := tsOwner(t).Migrate(ctx, store.TreeTimeseries); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, p *store.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := p.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func latest(t *testing.T, tree store.Tree) int64 {
	t.Helper()
	v, err := store.Latest(tree)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Both trees go up, down to nothing and up again against the real
// databases; each database holds only its own version table, and a tree
// run against the other's database is refused (E-01: the own tree on
// the same database is accepted).
func TestIntegrationTreesUpDownUp(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		tree store.Tree
		p    *store.Pool
	}{{store.TreeRelational, relOwner(t)}, {store.TreeTimeseries, tsOwner(t)}} {
		want := latest(t, c.tree)
		if _, err := c.p.Migrate(ctx, c.tree); err != nil {
			t.Fatalf("%s first up: %v", c.tree, err)
		}
		down, err := c.p.MigrateDown(ctx, c.tree, 0)
		if err != nil || int64(len(down)) != want {
			t.Fatalf("%s down: %d rolled back, %v", c.tree, len(down), err)
		}
		if v, err := c.p.SchemaVersion(ctx, c.tree); err != nil || v != 0 {
			t.Fatalf("%s after down: version %d %v", c.tree, v, err)
		}
		up, err := c.p.Migrate(ctx, c.tree)
		if err != nil || int64(len(up)) != want {
			t.Fatalf("%s up again: %d applied, %v", c.tree, len(up), err)
		}
		st, err := c.p.MigrationStatus(ctx, c.tree)
		if err != nil || int64(len(st)) != want {
			t.Fatalf("%s status: %v %v", c.tree, st, err)
		}
		for _, s := range st {
			if !s.Applied || s.AppliedAt.IsZero() {
				t.Errorf("%s status: %+v not applied", c.tree, s)
			}
		}
		own := count(t, c.p, "SELECT count(*) FROM pg_tables WHERE tablename = $1", c.tree.VersionTable())
		var otherTree store.Tree = store.TreeRelational
		if c.tree == store.TreeRelational {
			otherTree = store.TreeTimeseries
		}
		other := count(t, c.p, "SELECT count(*) FROM pg_tables WHERE tablename = $1", otherTree.VersionTable())
		if own != 1 || other != 0 {
			t.Errorf("%s database: %d own version tables, %d of the other tree", c.tree, own, other)
		}
		if _, err := c.p.Migrate(ctx, otherTree); !errors.Is(err, store.ErrWrongDatabase) {
			t.Errorf("%s tree on the %s database: %v", otherTree, c.tree, err)
		}
		if again, err := c.p.Migrate(ctx, c.tree); err != nil || len(again) != 0 {
			t.Errorf("%s up when current: %v %v", c.tree, again, err)
		}
	}
}

// Grants proven both ways (PLAN §8, 06 T7): as ussp_app an INSERT into
// events, intent_versions and conformance_states works and an UPDATE or
// DELETE is refused with 42501, on events also when addressed to the
// partition; an UPDATE on an ordinary table (flights) works.
func TestIntegrationAppendOnlyGrants(t *testing.T) {
	ensureSchemas(t)
	ctx := context.Background()
	app := relApp(t)
	st := appStore(t)
	suffix := fmt.Sprint(time.Now().UnixNano())

	var eventID int64
	if err := st.Tx(ctx, func(q *relational.Queries) error {
		var err error
		eventID, err = store.Audit(ctx, q, store.Event{ActorType: store.ActorSystem, ActorID: "grant-test", EntityType: "grant_test", EntityID: suffix, EventType: "inserted"})
		return err
	}); err != nil || eventID == 0 {
		t.Fatalf("audit insert as ussp_app: %d %v", eventID, err)
	}
	var partition string
	if err := app.QueryRow(ctx, "SELECT tableoid::regclass::text FROM events WHERE id = $1", eventID).Scan(&partition); err != nil {
		t.Fatal(err)
	}

	// The rows the append-only tables need: an operator, a client, an
	// intent, a flight (as ussp_app, which may write them).
	var operatorID, intentID, flightID string
	if err := app.QueryRow(ctx, `INSERT INTO operator_accounts (authority_registration_number, display_name, contact_email, status)
		VALUES ($1, 'Test operator', 'ops@example.invalid', 'active') RETURNING id::text`, "GEO-TEST-"+suffix).Scan(&operatorID); err != nil {
		t.Fatal(err)
	}
	clientID := "client-" + suffix
	if _, err := app.Exec(ctx, `INSERT INTO oauth_clients (client_id, operator_id, secret_hash, scopes, status)
		VALUES ($1, $2::uuid, 'x', '{ussp.intents}', 'active')`, clientID, operatorID); err != nil {
		t.Fatal(err)
	}
	if err := app.QueryRow(ctx, `INSERT INTO operational_intents (id, operator_id, client_id, uas_serial, local_state, volumes, volumes_amsl, envelope_geom, time_start, time_end)
		VALUES (gen_random_uuid(), $1::uuid, $2, 'TEST0001', 'pending_validation', '[]', '[]',
		        ST_GeogFromText('SRID=4326;POLYGON((44.7 41.7,44.8 41.7,44.8 41.8,44.7 41.7))'), now(), now() + interval '1 hour')
		RETURNING id::text`, operatorID, clientID).Scan(&intentID); err != nil {
		t.Fatal(err)
	}
	if err := app.QueryRow(ctx, "INSERT INTO flights (intent_id, uas_serial, started_at) VALUES ($1::uuid, 'TEST0001', now()) RETURNING id::text", intentID).Scan(&flightID); err != nil {
		t.Fatal(err)
	}

	// Presence: INSERT works on both append-only tables.
	for _, ins := range []struct{ sql, arg string }{
		{"INSERT INTO intent_versions (intent_id, version, actor, change_reason, snapshot) VALUES ($1::uuid, 1, 'staff-1', 'created', '{}')", intentID},
		{"INSERT INTO conformance_states (flight_id, at, state, policy_version) VALUES ($1::uuid, now(), 'conforming', 1)", flightID},
	} {
		if _, err := app.Exec(ctx, ins.sql, ins.arg); err != nil {
			t.Fatalf("insert as ussp_app: %s: %v", ins.sql, err)
		}
	}

	// Absence: every UPDATE and DELETE is refused with 42501.
	for _, sql := range []string{
		"UPDATE events SET event_type = 'rewritten' WHERE id = " + fmt.Sprint(eventID),
		"DELETE FROM events WHERE id = " + fmt.Sprint(eventID),
		"UPDATE " + partition + " SET event_type = 'rewritten' WHERE id = " + fmt.Sprint(eventID),
		"DELETE FROM " + partition + " WHERE id = " + fmt.Sprint(eventID),
		"UPDATE intent_versions SET change_reason = 'rewritten' WHERE intent_id = '" + intentID + "'",
		"DELETE FROM intent_versions WHERE intent_id = '" + intentID + "'",
		"UPDATE conformance_states SET state = 'unknown' WHERE flight_id = '" + flightID + "'",
		"DELETE FROM conformance_states WHERE flight_id = '" + flightID + "'",
		"TRUNCATE events",
	} {
		_, err := app.Exec(ctx, sql)
		if store.SQLState(err) != store.StateInsufficientPrivilege {
			t.Errorf("%s as ussp_app: %v, want 42501", sql, err)
		}
	}
	if n := count(t, app, "SELECT count(*) FROM events WHERE id = $1 AND event_type = 'inserted'", eventID); n != 1 {
		t.Errorf("the event row changed: %d", n)
	}

	// Twin: an ordinary table is updatable by the same role.
	tag, err := app.Exec(ctx, "UPDATE flights SET last_state = 'airborne' WHERE id = $1::uuid", flightID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("update flights as ussp_app: %v %v", tag, err)
	}
}

// Retention proven both ways: a peer_flights row 25 h old is gone after
// the retention job runs, one 23 h old is kept; the job's limit is the
// standard's (uspace-core f3411), not a tuning knob.
func TestIntegrationPeerFlightsRetention(t *testing.T) {
	ensureSchemas(t)
	ctx := context.Background()
	ts := tsOwner(t)
	q := timeseries.New(ts)
	peer := fmt.Sprint("peer-", time.Now().UnixNano())
	now := time.Now().UTC()
	for _, c := range []struct {
		id  string
		age time.Duration
	}{{"old-25h", 25 * time.Hour}, {"fresh-23h", 23 * time.Hour}} {
		if err := q.InsertPeerFlight(ctx, timeseries.InsertPeerFlightParams{PeerUss: peer, RidFlightID: c.id, RxTs: now.Add(-c.age), State: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"old-25h", "fresh-23h"} {
		if n, err := q.CountPeerFlights(ctx, timeseries.CountPeerFlightsParams{PeerUss: peer, RidFlightID: id}); err != nil || n != 1 {
			t.Fatalf("%s before the job: %d %v", id, n, err)
		}
	}
	var jobID int64
	var matches bool
	if err := ts.QueryRow(ctx, `SELECT job_id, (config->>'drop_after')::interval = make_interval(secs => $1)
		FROM timescaledb_information.jobs WHERE proc_name = 'policy_retention' AND hypertable_name = 'peer_flights'`,
		float64(f3411.NetDpMaxDataRetentionPeriodSeconds)).Scan(&jobID, &matches); err != nil {
		t.Fatal(err)
	}
	if !matches {
		t.Fatalf("the peer_flights retention is not f3411.NetDpMaxDataRetentionPeriodSeconds (%d s)", f3411.NetDpMaxDataRetentionPeriodSeconds)
	}
	if _, err := ts.Exec(ctx, "CALL run_job($1::integer)", jobID); err != nil {
		t.Fatalf("run_job %d: %v", jobID, err)
	}
	if n, _ := q.CountPeerFlights(ctx, timeseries.CountPeerFlightsParams{PeerUss: peer, RidFlightID: "old-25h"}); n != 0 {
		t.Errorf("the 25 h old row survived the retention job: %d", n)
	}
	if n, _ := q.CountPeerFlights(ctx, timeseries.CountPeerFlightsParams{PeerUss: peer, RidFlightID: "fresh-23h"}); n != 1 {
		t.Errorf("the 23 h old row was dropped: %d", n)
	}
	// Every other hypertable has its compression after 7 days and its
	// retention; peer_flights is never compressed.
	jobs := count(t, ts, `SELECT count(*) FROM timescaledb_information.jobs WHERE proc_name IN ('policy_retention', 'policy_compression')
		AND hypertable_name IN ('telemetry', 'manned_tracks', 'econspicuity_tracks', 'broadcast_tracks', 'traffic_products', 'conformance_samples')`)
	if jobs != 12 {
		t.Errorf("%d compression and retention jobs on the six 90-day hypertables, want 12", jobs)
	}
}

type policyProjector struct {
	mu   sync.Mutex
	err  error
	seen []policy.Record
}

func (p *policyProjector) ProjectPolicy(_ context.Context, r policy.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, r)
	return p.err
}

// B-09 against the database: a policy write whose projection fails is
// refused with the 503-shaped error and leaves no row and no event; one
// whose projection succeeds leaves one row and one audit event.
func TestIntegrationPolicyPutWithProjection(t *testing.T) {
	ensureSchemas(t)
	ctx := context.Background()
	st := appStore(t)
	app := relApp(t)
	proj := &policyProjector{err: errors.New("kv: bucket policy unavailable")}
	svc := policy.New(st, proj, nil)
	rows := func() int64 { return count(t, app, "SELECT count(*) FROM policy") }
	events := func() int64 { return count(t, app, "SELECT count(*) FROM events WHERE entity_type = 'policy'") }
	rows0, events0 := rows(), events()

	_, err := svc.Put(ctx, "staff-1", "integration: refused", policy.Defaults())
	var pe *policy.ProjectionError
	if !errors.As(err, &pe) {
		t.Fatalf("failing projector: %v", err)
	}
	if p := httpx.ProblemFromError(err); p.Status != 503 || p.Slug() != "projection_unavailable" {
		t.Fatalf("problem %+v", p)
	}
	if rows() != rows0 || events() != events0 || len(proj.seen) != 1 {
		t.Fatalf("refused write left rows %d->%d events %d->%d", rows0, rows(), events0, events())
	}

	proj.err = nil
	v := policy.Defaults()
	v.LostLinkS = 20
	r, err := svc.Put(ctx, "staff-1", "integration: accepted", v)
	if err != nil {
		t.Fatal(err)
	}
	if rows() != rows0+1 || events() != events0+1 {
		t.Fatalf("accepted write: rows %d->%d events %d->%d", rows0, rows(), events0, events())
	}
	if r.Version <= proj.seen[0].Version {
		t.Errorf("version %d reused the refused write's %d", r.Version, proj.seen[0].Version)
	}
	ev, err := st.Queries().ListEventsByEntity(ctx, relational.ListEventsByEntityParams{EntityType: "policy", EntityID: fmt.Sprint(r.Version)})
	if err != nil || len(ev) != 1 || ev[0].EventType != store.EventPolicyPut || ev[0].ActorID != "staff-1" {
		t.Fatalf("audit %+v %v", ev, err)
	}
	got, err := policy.New(st, proj, nil).Load(ctx)
	if err != nil || got.Version != r.Version || got.Values.LostLinkS != 20 {
		t.Fatalf("load %+v %v", got, err)
	}
}

// The outbox: idempotent enqueue, a claim bounded by MaxClaim (E-10),
// done and failed items, and a failed item due again only after its
// backoff.
func TestIntegrationOutbox(t *testing.T) {
	ensureSchemas(t)
	ctx := context.Background()
	st := appStore(t)
	if _, err := relOwner(t).Exec(ctx, "DELETE FROM dss_outbox"); err != nil {
		t.Fatal(err)
	}
	entity := fmt.Sprint("intent-", time.Now().UnixNano())
	var first, second bool
	if err := st.Tx(ctx, func(q *relational.Queries) error {
		var err error
		if first, err = store.Enqueue(ctx, q, store.OutboxOIRPut, entity, 1, map[string]any{"ovn": "a"}); err != nil {
			return err
		}
		second, err = store.Enqueue(ctx, q, store.OutboxOIRPut, entity, 1, map[string]any{"ovn": "a"})
		return err
	}); err != nil || !first || second {
		t.Fatalf("enqueue twice: %v %v %v", first, second, err)
	}
	// A rolled-back transaction queues nothing.
	_ = st.Tx(ctx, func(q *relational.Queries) error {
		if _, err := store.Enqueue(ctx, q, store.OutboxOIRPut, entity, 2, nil); err != nil {
			return err
		}
		return errors.New("roll back")
	})
	ob := st.Outbox()
	items, err := ob.Claim(ctx, 10)
	if err != nil || len(items) != 1 || items[0].EntityID != entity || items[0].Attempts != 1 {
		t.Fatalf("claim %+v %v", items, err)
	}
	if again, _ := ob.Claim(ctx, 10); len(again) != 0 {
		t.Fatalf("a leased item was claimed twice: %+v", again)
	}
	if err := ob.Fail(ctx, items[0].ID, errors.New("dss: 503"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := ob.Done(ctx, items[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := ob.Done(ctx, items[0].ID); !errors.Is(err, store.ErrNotPending) {
		t.Fatalf("done twice: %v", err)
	}

	// E-10: more due items than MaxClaim; one claim takes MaxClaim.
	if err := st.Tx(ctx, func(q *relational.Queries) error {
		for i := range store.MaxClaim + 20 {
			if _, err := store.Enqueue(ctx, q, store.OutboxPeerNotify, fmt.Sprint(entity, "-", i), 0, nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	big, err := ob.Claim(ctx, store.MaxClaim+20)
	if err != nil || len(big) != store.MaxClaim {
		t.Fatalf("claim beyond the bound: %d %v", len(big), err)
	}
	// A failed item with no backoff is due again at once; the rest wait.
	if err := ob.Fail(ctx, big[0].ID, nil, 0); err != nil {
		t.Fatal(err)
	}
	rest, err := ob.Claim(ctx, store.MaxClaim)
	if err != nil || len(rest) != 21 || !slices.ContainsFunc(rest, func(i store.OutboxItem) bool { return i.ID == big[0].ID && i.Attempts == 2 }) {
		t.Fatalf("after one failure: %d items %v", len(rest), err)
	}
}
