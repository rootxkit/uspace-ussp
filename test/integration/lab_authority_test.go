//go:build integration && lab

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/occurrence"
	occstore "github.com/rootxkit/uspace-ussp/internal/occurrence/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// The lab clause of H-1 (docs/RUNBOOKS/WP-15.md): this USSP's occurrence
// reports reach the real uspace-authority. The authority runs from its
// image (scripts/lab-authority.sh, test/e2e/authority/compose.yaml). Its
// first admin registers this USSP's machine client (ussp-USSPLAB-01,
// occurrences.write, audience the authority's host) and an incident
// officer; this USSP's own token client takes a token from the
// authority's token service; the Service, on this USSP's real database,
// detects an airprox and delivers it; the incident officer reads it on
// the authority's console API. Then the replay: the report put back to
// pending, as after an answer lost after the authority's commit or a
// restart before the outcome was recorded, is delivered again as the
// same occurrence (200, the first id), and the authority holds one;
// another body under the same report_ref is the authority's 409, which
// the client reports as permanent.
//
// Environment: LAB_AUTHORITY_URL, LAB_AUTHORITY_ADMIN_PASSWORD_FILE,
// LAB_AUTHORITY_IMAGE (logged).

// authorityGet is GET path on the authority with bearer, decoded into out.
func authorityGet(t *testing.T, base, path, bearer string, out any) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("GET %s: %v %.300s", path, err, b)
		}
	}
	if resp.StatusCode != http.StatusOK {
		t.Logf("GET %s: %d %.300s", path, resp.StatusCode, b)
	}
	return resp.StatusCode
}

// authorityOccurrences is every occurrence id the authority lists.
func authorityOccurrences(t *testing.T, base, bearer string) []string {
	t.Helper()
	var ids []string
	cursor := ""
	for range 100 {
		var page struct {
			Occurrences []struct {
				ID string `json:"occurrence_id"`
			} `json:"occurrences"`
			Next *string `json:"next_cursor"`
		}
		path := "/v1/occurrences?limit=200"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		if code := authorityGet(t, base, path, bearer, &page); code != http.StatusOK {
			t.Fatalf("list occurrences: %d", code)
		}
		for _, o := range page.Occurrences {
			ids = append(ids, o.ID)
		}
		if page.Next == nil {
			return ids
		}
		cursor = *page.Next
	}
	t.Fatal("more than 100 pages of occurrences")
	return nil
}

func TestLabAuthorityReceivesTheOccurrence(t *testing.T) {
	base := strings.TrimRight(labEnv(t, "LAB_AUTHORITY_URL"), "/")
	pwFile := labEnv(t, "LAB_AUTHORITY_ADMIN_PASSWORD_FILE")
	t.Logf("authority image %s at %s", os.Getenv("LAB_AUTHORITY_IMAGE"), base)
	ctx := context.Background()
	pw, err := os.ReadFile(pwFile)
	if err != nil {
		t.Fatal(err)
	}
	admin := signIn(t, base, "admin", strings.TrimSpace(string(pw)))

	// This USSP's machine client at the authority's token service. The
	// authority registers ussp-<code>-<nn> with an alphanumeric code
	// (M24), so the lab USSP's code has no hyphen.
	const systemID = "USSPLAB"
	aud, err := auth.AudienceOf(base)
	if err != nil {
		t.Fatal(err)
	}
	clientID := auth.ClientIDFor(systemID)
	code, body := postJSON(t, base+"/v1/oauth/clients", admin, map[string]any{"client_id": clientID, "scopes": []string{occurrence.ScopeOccurrences},
		"audiences": []string{aud}, "auth_method": "client_secret_post", "note": "H-1 lab run"})
	var created struct {
		Secret string `json:"client_secret"`
	}
	if code != http.StatusCreated || json.Unmarshal(body, &created) != nil || created.Secret == "" {
		t.Fatalf("create %s: %d %.300s", clientID, code, body)
	}
	// An incident officer reads what was received.
	officerPass := "lab-officer-" + unique()
	if code, body := postJSON(t, base+"/v1/users", admin, map[string]any{"username": "lab-officer", "password": officerPass,
		"roles": []string{"incident_officer"}, "realm": "console", "display_name": "Lab incident officer"}); code != http.StatusCreated {
		t.Fatalf("create officer: %d %.300s", code, body)
	}
	officer := signIn(t, base, "lab-officer", officerPass)
	before := authorityOccurrences(t, base, officer)

	tokens, err := auth.NewOutgoing(auth.OutgoingConfig{TokenURL: base + "/oauth/token", ClientID: clientID, ClientSecret: created.Secret})
	if err != nil {
		t.Fatal(err)
	}
	client, err := occurrence.NewClient(base, tokens, nil)
	if err != nil {
		t.Fatal(err)
	}
	counters := &core.Counters{}
	st := occstore.Store{S: appStore(t)}
	svc := &occurrence.Service{Store: st, Deliverer: client, Policy: policy.Defaults, SystemID: systemID,
		RecordsURL: "https://ussp.lab.test", Counters: counters, Logger: quiet()}

	// An airprox within the thresholds, detected and delivered.
	f := seedFlight(t, "activated", nil, time.Now().Add(-10*time.Minute))
	seedProximity(t, f, "pair-"+unique(), 20.0, 5.0, time.Now().Add(-time.Minute))
	if err := svc.Detect(ctx); err != nil {
		t.Fatal(err)
	}
	var ref string
	var payload []byte
	if err := appPool(t).QueryRow(ctx, "SELECT report_ref, payload FROM occurrence_reports WHERE $1::uuid = ANY(flight_ids)", f.flightID).Scan(&ref, &payload); err != nil {
		t.Fatal(err)
	}
	// Only this report is due: the others the database holds wait.
	if _, err := appPool(t).Exec(ctx, "UPDATE occurrence_reports SET next_at = now() + interval '1 hour' WHERE state = 'pending' AND report_ref <> $1", ref); err != nil {
		t.Fatal(err)
	}
	queued := time.Now()
	if n := svc.DeliverDue(ctx); n != 1 {
		var last *string
		_ = appPool(t).QueryRow(ctx, "SELECT last_error FROM occurrence_reports WHERE report_ref = $1", ref).Scan(&last)
		t.Fatalf("delivered %d; last_error %v; counters %v", n, deref(last), counters.Snapshot())
	}
	t.Logf("report %s delivered %v after the sweep began", ref, time.Since(queued).Round(time.Millisecond))
	var state string
	var authRef *string
	if err := appPool(t).QueryRow(ctx, "SELECT state, authority_ref FROM occurrence_reports WHERE report_ref = $1", ref).Scan(&state, &authRef); err != nil {
		t.Fatal(err)
	}
	if state != "delivered" || authRef == nil || *authRef == "" {
		t.Fatalf("state %s, authority_ref %v", state, deref(authRef))
	}

	// Visible to the authority: the incident officer reads the report.
	var got struct {
		ID         string `json:"occurrence_id"`
		Category   string `json:"category"`
		Channel    string `json:"channel"`
		Origin     string `json:"origin"`
		Within72h  bool   `json:"within_72h"`
		State      string `json:"state"`
		HasPerson  bool   `json:"has_reporter_person"`
		IntentRefs []string
		Aircraft   []struct {
			Serial      *string `json:"serial"`
			OperatorReg *string `json:"operator_reg"`
			FlightID    *string `json:"flight_id"`
		} `json:"aircraft"`
		Manned []struct {
			ICAO24 *string `json:"icao24"`
		} `json:"manned"`
		MinSeparation *struct {
			HM *float64 `json:"h_m"`
			VM *float64 `json:"v_m"`
		} `json:"min_separation"`
		EvidenceURLs []string `json:"evidence_urls"`
	}
	if code := authorityGet(t, base, "/v1/occurrences/"+*authRef, officer, &got); code != http.StatusOK {
		t.Fatalf("the officer cannot read %s: %d", *authRef, code)
	}
	t.Logf("at the authority: %s category %s channel %s origin %s within_72h %v state %s has_reporter_person %v", got.ID, got.Category,
		got.Channel, got.Origin, got.Within72h, got.State, got.HasPerson)
	if got.Category != occurrence.KindAirprox || got.Channel != occurrence.ChannelMandatory || got.Origin != "client" || !got.Within72h ||
		got.State != "received" || !got.HasPerson || len(got.Aircraft) != 1 || got.Aircraft[0].FlightID == nil || *got.Aircraft[0].FlightID != f.flightID ||
		got.Aircraft[0].OperatorReg == nil || *got.Aircraft[0].OperatorReg != f.regPublic || len(got.Manned) != 1 || *got.Manned[0].ICAO24 != "4ca1f0" ||
		got.MinSeparation == nil || got.MinSeparation.HM == nil || *got.MinSeparation.HM != 20 || len(got.EvidenceURLs) != 1 {
		t.Fatalf("the authority holds %+v", got)
	}
	after := authorityOccurrences(t, base, officer)
	if len(after) != len(before)+1 {
		t.Fatalf("the authority lists %d reports, %d before", len(after), len(before))
	}

	// The replay: the report back to pending (an answer lost after the
	// authority's commit, a restart before the outcome was stored) is
	// delivered again as the same occurrence; the authority holds one.
	if _, err := appPool(t).Exec(ctx, `UPDATE occurrence_reports SET state = 'pending', submitted_at = NULL, authority_ref = NULL, next_at = now()
		WHERE report_ref = $1`, ref); err != nil {
		t.Fatal(err)
	}
	if n := svc.DeliverDue(ctx); n != 1 {
		t.Fatalf("the replay delivered %d", n)
	}
	var again *string
	if err := appPool(t).QueryRow(ctx, "SELECT authority_ref FROM occurrence_reports WHERE report_ref = $1", ref).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if again == nil || *again != *authRef {
		t.Fatalf("the replay answered %v, first %s", deref(again), *authRef)
	}
	id, err := client.Submit(ctx, payload)
	if err != nil || id != *authRef {
		t.Fatalf("the same body again: %q %v", id, err)
	}
	if n := len(authorityOccurrences(t, base, officer)); n != len(after) {
		t.Fatalf("after two replays the authority lists %d reports, %d before them", n, len(after))
	}
	t.Logf("two replays answered %s; the authority lists %d reports, as before them", id, len(after))
	if counters.Get(occurrence.CounterFailed) != 0 || counters.Get(occurrence.CounterDelivered) != 2 {
		t.Fatalf("counters %v", counters.Snapshot())
	}

	// Another body under the same report_ref: the authority's 409,
	// permanent at the client (the Service fails it at once).
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatal(err)
	}
	m["narrative"] = "another report under the same reference"
	other, _ := json.Marshal(m)
	_, err = client.Submit(ctx, other)
	var perm *occurrence.PermanentError
	if !errors.As(err, &perm) || perm.Status != http.StatusConflict || !strings.Contains(err.Error(), "report_ref_conflict") {
		t.Fatalf("another body under %s: %v", ref, err)
	}
	t.Logf("another body under the same report_ref: %v", err)
	if n := len(authorityOccurrences(t, base, officer)); n != len(after) {
		t.Fatalf("after the conflict the authority lists %d reports", n)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%q", *s)
}
