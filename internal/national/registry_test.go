package national

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/registry"
)

// fakeRegistry answers every key valid from the cache, or fails.
type fakeRegistry struct {
	err   error
	actor string
	got   []registry.Query
	p     registry.Purpose
}

func (f *fakeRegistry) ValidateAudited(_ context.Context, actorType, actorID string, qs []registry.Query, p registry.Purpose) ([]registry.Result, error) {
	f.actor, f.got, f.p = actorType+":"+actorID, qs, p
	if f.err != nil {
		return nil, f.err
	}
	age := 12.5
	until := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	var r registry.Result
	if qs[0].Operator != "" {
		r.Operator = &registry.Answer{Key: "GEOTESTOP0001", Status: registry.StatusValid, ValidUntil: &until, CacheAgeS: &age}
	}
	if qs[0].Serial != "" {
		r.UAS = &registry.Answer{Key: qs[0].Serial, Status: registry.StatusUnknown, Reason: registry.ReasonRegistryUnavailable}
	}
	if qs[0].Pilot != "" {
		r.Pilot = &registry.Answer{Key: qs[0].Pilot, Status: registry.StatusValid, CacheAgeS: &age,
			Competencies: []registry.Competency{{Competency: "A2", ValidUntil: until}}}
	}
	return []registry.Result{r}, nil
}

// GET /v1/registry/validate answers an operator client holding
// ussp.intents with the cached status only, recorded with the client
// and the purpose; any other caller is refused before the lookup.
func TestValidateRegistryRoute(t *testing.T) {
	reg := &fakeRegistry{}
	h, iss := routerWith(t, reg)
	intents, err := iss.IssueOperator("client-7", []string{auth.ScopeIntents}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	geo, _ := iss.IssueOperator("client-7", []string{auth.ScopeGeo}, time.Hour, time.Now())
	sess, _, _ := iss.IssueSession("acc-1", auth.RealmPortal, "s1", []string{auth.RoleOperatorAdmin}, time.Hour, time.Now())
	call := func(query, bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/v1/registry/validate?"+query, nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	q := "purpose=authorisation&operator=GEOTESTOP0001-abc&serial=TEST-SN-A&pilot=GEO-TEST-PILOT-1"
	for name, c := range map[string]struct {
		token  string
		status int
	}{"no token": {"", 401}, "other scope": {geo.Token, 403}, "session": {sess, 403}} {
		if rec := call(q, c.token); rec.Code != c.status {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if reg.got != nil {
		t.Fatal("a refused caller reached the lookup")
	}
	rec := call(q, intents.Token)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if reg.actor != "client:client-7" || reg.p != registry.PurposeAuthorisation || reg.got[0].Operator != "GEOTESTOP0001-abc" {
		t.Fatalf("asked %s %s %+v", reg.actor, reg.p, reg.got)
	}
	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["operator"]["status"] != "valid" || body["operator"]["cache_age_s"] != 12.5 || body["operator"]["key"] != "GEOTESTOP0001" ||
		body["uas"]["reason"] != "registry_unavailable" || body["uas"]["cache_age_s"] != nil ||
		len(body["pilot"]["competencies"].([]any)) != 1 {
		t.Fatalf("body %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "-abc") {
		t.Fatal("the secret part was echoed")
	}
	if rec := call("serial=TEST-SN-A", intents.Token); rec.Code != 400 {
		t.Fatalf("no purpose: %d %s", rec.Code, rec.Body.String())
	}
	reg.err = &registry.AuditError{Err: errors.New("db down")}
	if rec := call(q, intents.Token); rec.Code != 503 || !strings.Contains(rec.Body.String(), "audit_unavailable") || strings.Contains(rec.Body.String(), "db down") {
		t.Fatalf("audit failure: %d %s", rec.Code, rec.Body.String())
	}
	h, iss = routerWith(t, nil)
	intents, _ = iss.IssueOperator("client-7", []string{auth.ScopeIntents}, time.Hour, time.Now())
	r := httptest.NewRequest("GET", "/v1/registry/validate?"+q, nil)
	r.Header.Set("Authorization", "Bearer "+intents.Token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "registry_unavailable") {
		t.Fatalf("no registry: %d %s", rec.Code, rec.Body.String())
	}
}
