//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// seeded is one operator, client, authorised intent and flight written
// as ussp_app, as the WP-15 tests need them (records, coordination,
// occurrences).
type seeded struct {
	operatorID, clientID, intentID, flightID, number, serial string
	// regPublic is the public part of the operator registration number
	// the intent and the flight carry (its secret part is "-abc").
	regPublic  string
	start, end time.Time
}

// seedVolumes is the stored F3548 Volume4D list of a seeded intent.
func seedVolumes(start, end time.Time) string {
	return fmt.Sprintf(`[{"volume":{"outline_polygon":{"vertices":[{"lat":41.71,"lng":44.78},{"lat":41.71,"lng":44.79},`+
		`{"lat":41.72,"lng":44.79},{"lat":41.72,"lng":44.78}]},"altitude_lower":{"value":520,"reference":"W84","units":"M"},`+
		`"altitude_upper":{"value":570,"reference":"W84","units":"M"}},"time_start":{"value":"%s","format":"RFC3339"},`+
		`"time_end":{"value":"%s","format":"RFC3339"}}]`, start.Format(time.RFC3339), end.Format(time.RFC3339))
}

// seedFlight writes an intent in localState over U-space airspace ids
// with an activated version, and a flight of it started at start.
func seedFlight(t *testing.T, localState string, airspaces []string, start time.Time) seeded {
	t.Helper()
	ensureSchemas(t)
	if airspaces == nil {
		airspaces = []string{}
	}
	ctx := context.Background()
	app := appPool(t)
	u := unique()
	s := seeded{number: "GEO-TEST-" + u, serial: "TEST" + u, clientID: "client-wp15-" + u, start: start.UTC().Truncate(time.Second)}
	digits := strings.Repeat("0", 13) + u
	s.regPublic = "GEO" + digits[len(digits)-13:]
	s.end = s.start.Add(time.Hour)
	if err := app.QueryRow(ctx, `INSERT INTO operator_accounts (authority_registration_number, display_name, contact_email, status)
		VALUES ($1, 'WP-15 operator', 'wp15@example.invalid', 'active') RETURNING id::text`, s.number).Scan(&s.operatorID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, `INSERT INTO oauth_clients (client_id, operator_id, secret_hash, scopes, status)
		VALUES ($1, $2::uuid, 'x', '{ussp.intents}', 'active')`, s.clientID, s.operatorID); err != nil {
		t.Fatal(err)
	}
	if err := app.QueryRow(ctx, `INSERT INTO operational_intents (id, operator_id, client_id, uas_serial, local_state, dss_state, volumes, volumes_amsl,
		    envelope_geom, time_start, time_end, authorisation_number, decision, uspace_airspace_ids, in_uspace_airspace, policy_version,
		    deviation_thresholds, cis_version_checked, registry_checked_at, operator_reg, mode, category, flight_type)
		VALUES (gen_random_uuid(), $1::uuid, $2, $3, $4, 'Activated', $5::jsonb, '[]',
		        ST_GeogFromText('SRID=4326;POLYGON((44.78 41.71,44.79 41.71,44.79 41.72,44.78 41.72,44.78 41.71))'), $6, $7,
		        $8, 'authorised', $9::text[], cardinality($9::text[]) > 0, 1, '{"h_m":50,"v_m":15,"t_s":60}', 'uspace_airspace:3', now(),
		        $10, 'BVLOS', 'specific', 'normal')
		RETURNING id::text`, s.operatorID, s.clientID, s.serial, localState, seedVolumes(s.start, s.end), s.start, s.end,
		"GE-USSP-DEV-"+u, airspaces, s.regPublic+"-abc").Scan(&s.intentID); err != nil {
		t.Fatal(err)
	}
	for v, st := range []string{"accepted", "activated"} {
		if _, err := app.Exec(ctx, `INSERT INTO intent_versions (intent_id, version, at, actor, change_reason, snapshot)
			VALUES ($1::uuid, $2, $3, $4, $5, jsonb_build_object('decision', jsonb_build_object('state', $6::text, 'decision', 'authorised',
			        'authorisation_number', $7::text, 'deviation_thresholds', '{"h_m":50,"v_m":15,"t_s":60}'::jsonb, 'conflicts', '[]'::jsonb,
			        'cis_version_checked', 'uspace_airspace:3', 'registry_checked_at', now(), 'policy_version', 1),
			        'request', jsonb_build_object('uas_serial', $8::text, 'operator_reg', $9::text, 'loss_of_c2_procedure', 'call Nino on 555-0100')))`,
			s.intentID, v+1, s.start.Add(time.Duration(v-2)*time.Minute), s.clientID, st, st, "GE-USSP-DEV-"+u, s.serial, s.regPublic+"-abc"); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.QueryRow(ctx, `INSERT INTO flights (intent_id, authorisation_number, uas_serial, operator_reg, client_id, started_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6) RETURNING id::text`, s.intentID, "GE-USSP-DEV-"+u, s.serial, s.regPublic+"-abc", s.clientID, s.start).Scan(&s.flightID); err != nil {
		t.Fatal(err)
	}
	return s
}

// seedState appends a conformance state with its numbers and last
// position and returns its id.
func seedState(t *testing.T, flightID, state, reason string, at time.Time, distM, heightM float64) int64 {
	t.Helper()
	var id int64
	if err := appPool(t).QueryRow(context.Background(), `INSERT INTO conformance_states (flight_id, at, state, reason, distance_outside_m,
		    height_over_m, policy_version, last_lat_deg, last_lng_deg)
		VALUES ($1::uuid, $2, $3, NULLIF($4, ''), $5, $6, 1, 41.7155, 44.7955) RETURNING id`, flightID, at, state, reason, distM, heightM).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// endIntent ends the seeded intent (as the intent service would).
func endIntent(t *testing.T, intentID string) {
	t.Helper()
	if _, err := appPool(t).Exec(context.Background(), "UPDATE operational_intents SET local_state = 'ended', updated_at = now() WHERE id = $1::uuid", intentID); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls cond every 50 ms until it holds, or fails the test
// after d.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", d, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
