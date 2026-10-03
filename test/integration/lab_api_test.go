//go:build integration && lab

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/f3548"

	"github.com/rootxkit/uspace-ussp/internal/app/api"
	"github.com/rootxkit/uspace-ussp/internal/auth"
	"github.com/rootxkit/uspace-ussp/internal/dss"
	"github.com/rootxkit/uspace-ussp/internal/intent"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/cisp"
)

// TestLabAPIProcess runs the api process itself (internal/app/api, as
// cmd/api runs it: its DSS client, availability, writer, USS endpoints
// and exchange log as api wires them) against the lab's InterUSS DSS,
// its token service and its second USSP, for one intent end to end:
// filed, written to the DSS and authorised, activated, ended and deleted.
// The CISP and the authority's registry are the fakes of the other
// api-process tests (the lab has neither yet).
//
// The lab issuer knows this USSP as LAB_CLIENT_ID (ussp-USSP-DEV-01); the
// api asks for its tokens as auth.ClientIDFor(USSP_SYSTEM_ID)
// (ussp-ussp-dev-01, M24). A broker in front of the lab issuer takes the
// api's client and asks the lab issuer as LAB_CLIENT_ID for every DSS and
// peer audience, so the tokens the DSS sees carry the lab's sub while the
// configured id differs: the manager the DSS records is not the one the
// configuration derives. Every other audience (the fakes) gets the fakes'
// bearer, as in the other api-process tests.
//
// Environment: the lab_test.go variables LAB_DSS_URL, LAB_TOKEN_URL,
// LAB_ISSUER, LAB_JWKS, LAB_CLIENT_ID, LAB_CLIENT_SECRET, LAB_SELF_HOST,
// LAB_LISTEN, LAB_SIM_INTENT.
func TestLabAPIProcess(t *testing.T) {
	dssURL, tokenURL := labEnv(t, "LAB_DSS_URL"), labEnv(t, "LAB_TOKEN_URL")
	issuer, jwks := labEnv(t, "LAB_ISSUER"), labEnv(t, "LAB_JWKS")
	clientID, secret := labEnv(t, "LAB_CLIENT_ID"), labEnv(t, "LAB_CLIENT_SECRET")
	self, listen, simIntent := labEnv(t, "LAB_SELF_HOST"), labEnv(t, "LAB_LISTEN"), labEnv(t, "LAB_SIM_INTENT")
	ctx := context.Background()
	ensureSchemas(t)
	cleanCIS(t)
	cleanDSSOutbox(t)
	t.Cleanup(func() { cleanDSSOutbox(t) })

	derived := auth.ClientIDFor("USSP-DEV")
	if derived == clientID {
		t.Fatalf("the configured id %s is the lab's sub: the run would not tell them apart", derived)
	}
	lab, err := auth.NewOutgoing(auth.OutgoingConfig{TokenURL: tokenURL, ClientID: clientID, ClientSecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	dssHost, err := auth.AudienceOf(dssURL)
	if err != nil {
		t.Fatal(err)
	}
	a := newFakeAuthority(t)
	var forwarded, refusedClient atomic.Int32
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
			_ = json.NewEncoder(w).Encode(a.iss.JWKS())
			return
		}
		if err := r.ParseForm(); err != nil || r.PostForm.Get("client_id") != derived {
			refusedClient.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		aud := r.PostForm.Get("audience")
		tok := cisp.Token
		if aud == dssHost || strings.HasPrefix(aud, "sim-ussp") || aud == self {
			lt, err := lab.TokenFor(r.Context(), aud, strings.Fields(r.PostForm.Get("scope")))
			if err != nil {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			forwarded.Add(1)
			tok = lt
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": 300})
	}))
	t.Cleanup(broker.Close)

	// The api process's configuration: withAuth's, with the lab's DSS,
	// the broker as the token service and the lab issuer allow-listed for
	// the peers' calls.
	_, _, keyPEM := testKeys(t)
	keyFile := filepath.Join(t.TempDir(), "issuer-key.pem")
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{
		"USSP_API_ADDR":        listen,
		"USSP_PG_URL":          mustEnv(t, "USSP_TEST_PG_URL"),
		"USSP_TS_URL":          mustEnv(t, "USSP_TEST_TS_URL"),
		"USSP_NATS_URL":        mustEnv(t, "USSP_TEST_NATS_URL"),
		"USSP_ISSUER_KEY_FILE": keyFile,
		"USSP_AUDIENCES":       testHost + "," + testAlias + "," + self,
		"USSP_TOKEN_ISSUERS":   a.url + "=" + broker.URL + "/.well-known/jwks.json," + issuer + "=" + jwks,
		"USSP_CIS_RECONCILE_S": "5",
	}
	cis := withCIS(t, vars)
	reg := withRegistry(t, vars)
	withGeoid(t, vars)
	base := "http://" + self + listen
	vars["USSP_USS_BASE_URL"] = base
	vars["USSP_DSS_BASE_URL"] = dssURL
	// The U-space airspace around the lab's peer (SIM_USSP_INTENT, a 300 m
	// circle at 41.73, 44.85, 600 to 760 m W84), published before api
	// starts so its first pull has it.
	airspace := [4]float64{41.60, 44.70, 41.90, 45.00}
	cis.Publish("zones")
	cis.Publish("uspace_airspace", edFeature("TSA-LAB", "USPACE", airspace, 0, 3000, "AMSL", requirementsExt(nil)))
	cis.Publish("restrictions")

	start := time.Now()
	_ = runAt(t, api.Spec, vars)
	s := &stack{t: t, url: "http://127.0.0.1" + listen}
	took := within(t, 60*time.Second, func() bool {
		r := s.call("GET", "/readyz", nil, nil)
		deps, _ := r.body["dependencies"].(map[string]any)
		up := func(name string) bool {
			d, _ := deps[name].(map[string]any)
			st, _ := d["state"].(string)
			return st == "up"
		}
		return up("dss") && up("cis") && up("registry")
	})
	t.Logf("lab api: /readyz dss, cis and registry up %s after the start (%s after the process was asked to start)",
		took.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))

	// An operator, its client and a serial, as an operator's portal would.
	u := unique()
	number, serial := "GEO-TEST-"+u, "TEST"+u
	reg.SetOperator(number, "active", nil)
	reg.SetUAS(serial, "active", "", "")
	user, pass := "lab."+u, "lab-password-"+u
	r := s.call("POST", "/v1/accounts/operators", map[string]any{
		"registration_number": number, "display_name": "Lab operator " + u, "contact_email": "lab" + u + "@example.test",
		"admin_username": user, "admin_password": pass,
	}, nil)
	if r.status != 201 {
		t.Fatalf("register: %d %s", r.status, r.raw)
	}
	l := s.login("portal", user, pass, "")
	if l.status != 200 {
		t.Fatalf("login: %d %s", l.status, l.raw)
	}
	id, csecret := s.client(r.str("id"), l.str("token"), "ussp.intents")
	if b := s.call("POST", "/v1/accounts/operators/"+r.str("id")+"/clients/"+id+"/serials", map[string]any{"serial": serial}, bearer(l.str("token"))); b.status != 201 {
		t.Fatalf("bind: %d %s", b.status, b.raw)
	}
	tr := s.token(id, csecret)
	if tr.status != 200 {
		t.Fatalf("token: %d %s", tr.status, tr.raw)
	}
	token := tr.str("access_token")
	ir := &intentRig{t: t}
	file := func(ref string, box [4]float64, lo, hi float64) resp {
		st := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
		return s.call("POST", "/v1/intents", ir.request(number, serial, ref, box, "volumes", []any{ir.volume(box, lo, hi, st, st.Add(30*time.Minute))}), bearer(token))
	}
	// The DSS as this USSP reads it, with the lab's own credentials.
	dc := &dss.Client{DSSBaseURL: dssURL, Tokens: lab}

	// 1. The peer first: refused naming the lab peer's intent, whose
	// details the api read from sim-ussp with a lab token.
	simBox := [4]float64{41.7285, 44.8485, 41.7315, 44.8515}
	t0 := time.Now()
	p := file("lab-api-peer", simBox, 620, 740)
	t.Logf("lab api: intent over the peer's: %s in %s, conflicts %v", p.str("decision"), time.Since(t0).Round(time.Millisecond), refsOf(p))
	if p.str("decision") != "rejected" || !slices.Contains(refsOf(p), intent.PeerPrefix+simIntent) {
		t.Fatalf("%s", p.raw)
	}

	// 2. Clear of it: written to the DSS and authorised in the request.
	clearBox := [4]float64{41.7600, 44.8800, 41.7630, 44.8830}
	t0 = time.Now()
	b := file("lab-api-clear", clearBox, 620, 740)
	took = time.Since(t0)
	if b.str("decision") != "authorised" || b.str("dss_state") != "Accepted" {
		t.Fatalf("%s", b.raw)
	}
	bid := b.str("intent_id")
	ref, err := dc.GetOperationalIntent(ctx, bid)
	if err != nil || ref.State != f3548.Accepted || ref.Ovn == nil || ref.UssBaseUrl != base {
		t.Fatalf("the DSS holds %+v %v", ref, err)
	}
	t.Logf("lab api: intent %s authorised in %s; the DSS holds version %d under manager %s (configured client id %s), uss_base_url %s",
		bid, took.Round(time.Millisecond), ref.Version, ref.Manager, derived, ref.UssBaseUrl)
	if ref.Manager != clientID {
		t.Fatalf("the DSS records %s, not the lab's sub %s", ref.Manager, clientID)
	}

	// 3. Our identity as the api process used it: the availability polled
	// at the manager the DSS records, read from its exchange log.
	var availURL string
	within(t, 90*time.Second, func() bool {
		row := relOwner(t).QueryRow(ctx, `SELECT url FROM dss_exchanges WHERE recorder_role = 'Client' AND url LIKE '%/dss/v1/uss_availability/%' ORDER BY id DESC LIMIT 1`)
		return row.Scan(&availURL) == nil
	})
	t.Logf("lab api: availability polled at %s", availURL)
	if !strings.HasSuffix(availURL, "/dss/v1/uss_availability/"+clientID) {
		t.Fatalf("the availability of another id was polled: %s", availURL)
	}

	// 4. Activation and end mirrored in the DSS.
	if r := s.call("PATCH", "/v1/intents/"+bid, map[string]any{"action": "activate"}, bearer(token)); r.str("state") != "activated" {
		t.Fatalf("%s", r.raw)
	}
	took = within(t, 10*time.Second, func() bool {
		r, err := dc.GetOperationalIntent(ctx, bid)
		return err == nil && r.State == f3548.Activated
	})
	t.Logf("lab api: Activated in the DSS %s after the activation", took.Round(time.Millisecond))
	if r := s.call("PATCH", "/v1/intents/"+bid, map[string]any{"action": "end"}, bearer(token)); r.str("state") != "ended" {
		t.Fatalf("%s", r.raw)
	}
	took = within(t, 10*time.Second, func() bool {
		_, err := dc.GetOperationalIntent(ctx, bid)
		return err != nil && strings.Contains(err.Error(), "not found")
	})
	t.Logf("lab api: deleted from the DSS %s after the end", took.Round(time.Millisecond))

	// 5. What the api recorded: its exchanges for the intent, the DSS and
	// peer tokens the broker took from the lab issuer, and none refused.
	var n int
	if err := relOwner(t).QueryRow(ctx, `SELECT count(*) FROM dss_exchanges WHERE entity_id = $1`, bid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	t.Logf("lab api: %d exchanges recorded for %s; %d lab tokens brokered, %d token requests refused", n, bid, forwarded.Load(), refusedClient.Load())
	if forwarded.Load() == 0 || refusedClient.Load() != 0 {
		t.Fatal("the api's tokens did not come through the broker")
	}
}
