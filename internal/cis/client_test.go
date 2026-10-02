package cis

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rootxkit/uspace-ussp/internal/testfakes/cisp"
)

func TestNewClientRefusesBadBase(t *testing.T) {
	for _, u := range []string{"", "ftp://x", "/relative", "https://"} {
		if _, err := NewClient(ClientConfig{BaseURL: u}); err == nil {
			t.Fatalf("%q accepted", u)
		}
	}
}

func TestClientWithoutTokens(t *testing.T) {
	fake, err := cisp.New()
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	c, err := NewClient(ClientConfig{BaseURL: fake.URL()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetDataset(t.Context(), Zones, ""); !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("got %v", err)
	}
	if fake.TotalRequests() != 0 {
		t.Fatal("an unauthenticated request was sent")
	}
}

type failingTokens struct{}

func (failingTokens) Token(context.Context, string, ...string) (string, error) {
	return "", errors.New("token service down")
}

func TestClientTokenFailure(t *testing.T) {
	c, err := NewClient(ClientConfig{BaseURL: "https://cisp.test", Tokens: failingTokens{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetDataset(t.Context(), Zones, ""); err == nil || !strings.Contains(err.Error(), "token service down") {
		t.Fatalf("got %v", err)
	}
}

func TestClientReads(t *testing.T) {
	fake, err := cisp.New()
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	c, err := NewClient(ClientConfig{BaseURL: fake.URL() + "/", Tokens: cisp.Tokens{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if c.Host() != fake.Host() {
		t.Fatalf("host %q", c.Host())
	}
	// No version yet: 404 no_version is an answer, not an error.
	if f, err := c.GetDataset(ctx, Zones, ""); err != nil || f.Status != http.StatusNotFound {
		t.Fatalf("no version: %+v %v", f, err)
	}
	fake.Publish("zones", prohibited("TZP001").json())
	f, err := c.GetDataset(ctx, Zones, "")
	if err != nil || f.Status != http.StatusOK || f.Version != 1 || f.ETag != `"zones:1"` || len(f.Body) == 0 {
		t.Fatalf("v1: %+v %v", f, err)
	}
	if f, err := c.GetDataset(ctx, Zones, `"zones:1"`); err != nil || f.Status != http.StatusNotModified {
		t.Fatalf("304: %+v %v", f, err)
	}
	if f, err := c.GetVersion(ctx, Zones, 1); err != nil || f.Status != http.StatusOK || f.Version != 1 {
		t.Fatalf("version: %+v %v", f, err)
	}
	var se *StatusError
	if _, err := c.GetVersion(ctx, Zones, 9); !errors.As(err, &se) || se.Status != http.StatusNotFound || se.Slug != "not_found" {
		t.Fatalf("missing version: %v", err)
	}
	cl, err := c.Changes(ctx, 0, Zones)
	if err != nil || len(cl.Changes) != 1 || cl.Next != 1 {
		t.Fatalf("changes: %+v %v", cl, err)
	}
	if _, err := c.GetURL(ctx, "https://elsewhere.test/v1/zones"); err == nil {
		t.Fatal("read a pull_url on another host")
	}
	fake.Down()
	if _, err := c.GetDataset(ctx, Zones, ""); !errors.As(err, &se) || se.Status != http.StatusServiceUnavailable ||
		!strings.Contains(se.Error(), "503 unavailable: the fake CISP is down") {
		t.Fatalf("down: %v", err)
	}
	if _, err := c.Changes(ctx, 0, ""); err == nil {
		t.Fatal("changes while down")
	}
}

// A pull_url is followed only on the CISP's scheme, host and port, and
// only over https: each refused one makes no request, and the one on
// the CISP is read (a delta answer).
func TestClientPullURLGuard(t *testing.T) {
	fake, err := cisp.NewTLS()
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	c, err := NewClient(ClientConfig{BaseURL: fake.URL(), Tokens: cisp.Tokens{}, HTTPClient: fake.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	ch := fake.Publish("zones", prohibited("TZP001").json())
	fake.Publish("zones", prohibited("TZP001").json(), prohibited("TZP002").json())
	base, _ := url.Parse(fake.URL())
	refused := map[string]string{
		"plain http, same host and port": "http://" + base.Host + "/v1/zones?since_version=1",
		"another port":                   "https://" + base.Hostname() + ":1/v1/zones?since_version=1",
		"the default port":               "https://" + base.Hostname() + "/v1/zones?since_version=1",
		"another host":                   "https://elsewhere.test:" + base.Port() + "/v1/zones?since_version=1",
		"user information":               "https://u:p@" + base.Host + "/v1/zones?since_version=1",
		"relative":                       "/v1/zones?since_version=1",
		"another scheme":                 "ftp://" + base.Host + "/v1/zones",
	}
	before := fake.TotalRequests()
	for name, raw := range refused {
		if _, err := c.GetURL(ctx, raw); !errors.Is(err, ErrPullURLRefused) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if n := fake.TotalRequests() - before; n != 0 {
		t.Fatalf("refused pull_urls made %d requests", n)
	}
	f, err := c.GetURL(ctx, ch.PullURL)
	if err != nil || f.Status != http.StatusOK || f.Version != 2 || !strings.Contains(string(f.Body), `"from_version":0`) {
		t.Fatalf("the CISP's own pull_url: %+v %v", f, err)
	}
	if n := fake.Requests("GET /v1/zones"); n != 1 {
		t.Fatalf("%d reads of the delta", n)
	}
}

// An http base URL (a lab CISP) never has a pull_url followed, not even
// its own: the dataset is read whole from the base URL instead. A
// default port written out is the same port.
func TestClientPullURLPlainHTTPBase(t *testing.T) {
	fake, err := cisp.New()
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	c, err := NewClient(ClientConfig{BaseURL: fake.URL(), Tokens: cisp.Tokens{}})
	if err != nil {
		t.Fatal(err)
	}
	ch := fake.Publish("zones", prohibited("TZP001").json())
	if _, err := c.GetURL(t.Context(), ch.PullURL); !errors.Is(err, ErrPullURLRefused) || fake.TotalRequests() != 0 {
		t.Fatalf("http pull_url: %v, %d requests", err, fake.TotalRequests())
	}
	tls, err := NewClient(ClientConfig{BaseURL: "https://cisp.test", Tokens: cisp.Tokens{}})
	if err != nil {
		t.Fatal(err)
	}
	if u, err := tls.checkPullURL("https://CISP.test:443/v1/zones?since_version=1"); err != nil || u.Port() != "443" {
		t.Fatalf("explicit default port: %v", err)
	}
}

// Every answer is bounded and checked: a body over the cap, a version
// header that is not a version, a redirect.
func TestClientBounds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/zones":
			_, _ = w.Write([]byte(strings.Repeat("x", 2048)))
		case "/v1/restrictions":
			w.Header().Set("X-CIS-Version", "seven")
			_, _ = w.Write([]byte("{}"))
		case "/v1/uspace_airspace":
			http.Redirect(w, r, "https://elsewhere.test/", http.StatusFound)
		case "/v1/changes":
			_, _ = w.Write([]byte("not json"))
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()
	c, err := NewClient(ClientConfig{BaseURL: srv.URL, Tokens: cisp.Tokens{}, MaxBodyBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := c.GetDataset(ctx, Zones, ""); err == nil || !strings.Contains(err.Error(), "longer than 1024 bytes") {
		t.Fatalf("cap: %v", err)
	}
	if _, err := c.GetDataset(ctx, Restrictions, ""); err == nil || !strings.Contains(err.Error(), "X-CIS-Version") {
		t.Fatalf("version header: %v", err)
	}
	var se *StatusError
	if _, err := c.GetDataset(ctx, USpaceAirspace, ""); !errors.As(err, &se) || se.Status != http.StatusFound {
		t.Fatalf("redirect followed: %v", err)
	}
	if _, err := c.Changes(ctx, 0, ""); err == nil || !strings.Contains(err.Error(), "does not decode") {
		t.Fatalf("changes: %v", err)
	}
	if _, err := c.GetDataset(ctx, USSPList, ""); !errors.As(err, &se) || se.Status != http.StatusTeapot || se.Slug != "" {
		t.Fatalf("418: %v", err)
	}
}

func TestSinceVersionAndHost(t *testing.T) {
	if v, ok := sinceVersion("https://c.test/v1/zones?since_version=12"); !ok || v != 12 {
		t.Fatal("since_version 12")
	}
	for _, u := range []string{"https://c.test/v1/zones", "https://c.test/v1/zones?since_version=-1", "%zz"} {
		if _, ok := sinceVersion(u); ok {
			t.Fatalf("%q read", u)
		}
	}
	if hostOf("https://c.test:8443/x") != "c.test" || hostOf("/relative") != "" || hostOf("%zz") != "" {
		t.Fatal("hostOf")
	}
}

func TestParseVersion(t *testing.T) {
	good := collection(Zones, 3, prohibited("TZP001").json())
	v, rf := ParseVersion(Zones, good, `"zones:3"`, 3)
	if rf != nil || v.Number != 3 || v.Meta.CISDataset != "zones" || v.Meta.Issued == nil || len(v.Meta.Provider) != 1 || v.Meta.CISUpdatedAt == nil {
		t.Fatalf("good: %+v %v", v, rf)
	}
	cases := map[string]struct {
		body   []byte
		header int64
		want   string
	}{
		"header":       {good, 4, "is not X-CIS-Version 4"},
		"dataset":      {collection(Restrictions, 3), 0, `"restrictions" is not "zones"`},
		"no dataset":   {[]byte(`{"type":"FeatureCollection","features":[],"cis_version":1}`), 0, "cis_dataset: missing"},
		"no version":   {[]byte(`{"type":"FeatureCollection","features":[],"cis_dataset":"zones"}`), 0, "cis_version: missing"},
		"zero version": {[]byte(`{"type":"FeatureCollection","features":[],"cis_dataset":"zones","cis_version":0}`), 0, "cis_version: missing"},
		"updated":      {[]byte(`{"type":"FeatureCollection","features":[],"cis_dataset":"zones","cis_version":1,"cis_updated_at":"yesterday"}`), 0, "cis_updated_at"},
		"not json":     {[]byte(`not json`), 0, ""},
	}
	for name, c := range cases {
		_, rf := ParseVersion(Zones, c.body, "", c.header)
		if rf == nil || !strings.Contains(rf.Error(), c.want) {
			t.Fatalf("%s: %v", name, rf)
		}
	}
	stale := []byte(`{"type":"FeatureCollection","features":[],"cis_dataset":"restrictions","cis_version":2,"cis_publisher_stale_since":null}`)
	if v, rf := ParseVersion(Restrictions, stale, "", 0); rf != nil || string(v.Meta.PublisherStaleSince) != "null" {
		t.Fatalf("stale since: %+v %v", v, rf)
	}
}

func TestParseUSSPList(t *testing.T) {
	mk := func(m map[string]any) []byte {
		base := map[string]any{"schema": "cis/ussp_list/v1", "issued": "2026-10-01T00:00:00Z", "ussps": []any{},
			"cis_dataset": "ussp_list", "cis_version": 2, "cis_updated_at": "2026-10-01T00:00:00Z"}
		for k, v := range m {
			if v == nil {
				delete(base, k)
			} else {
				base[k] = v
			}
		}
		b, _ := json.Marshal(base)
		return b
	}
	if v, rf := ParseVersion(USSPList, mk(nil), "", 2); rf != nil || v.Number != 2 || v.Meta.CISUpdatedAt == nil {
		t.Fatalf("good: %v", rf)
	}
	for name, c := range map[string]struct {
		body   []byte
		header int64
	}{
		"unknown member": {mk(map[string]any{"colour": "red"}), 0},
		"schema":         {mk(map[string]any{"schema": "cis/other/v1"}), 0},
		"dataset":        {mk(map[string]any{"cis_dataset": nil}), 0},
		"version":        {mk(map[string]any{"cis_version": nil}), 0},
		"header":         {mk(nil), 3},
		"trailing":       {append(mk(nil), []byte(" {}")...), 0},
		"too large":      {[]byte(strings.Repeat(" ", ProblemLimits.MaxBytes+1)), 0},
	} {
		if _, rf := ParseVersion(USSPList, c.body, "", c.header); rf == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestDatasets(t *testing.T) {
	for _, d := range AllDatasets {
		if got, ok := ParseDataset(string(d)); !ok || got != d {
			t.Fatalf("%s", d)
		}
	}
	if _, ok := ParseDataset("weather"); ok {
		t.Fatal("weather is a dataset")
	}
	if USSPList.ED318() || !Zones.ED318() {
		t.Fatal("ED318")
	}
	if s := short(strings.Repeat("é", 100)); len([]rune(s)) != 83 {
		t.Fatalf("short: %d", len([]rune(s)))
	}
	if (&RefusalError{Dataset: Zones, Version: 2, First: "x", Problems: 3}).Error() != "zones version 2 refused: x (3 problems)" {
		t.Fatal("refusal text")
	}
}

func TestMergeDelta(t *testing.T) {
	clk := newClock()
	cur := mustVersion(t, Zones, 4, prohibited("TZP001").json(), prohibited("TZP002").json())
	e := loaded(t, clk, cur)
	held := e.Snapshot().Entries(Zones)
	changed := prohibited("TZP002")
	changed.upper = f64(50)
	delta := func(m map[string]any) []byte {
		base := map[string]any{"dataset": "zones", "from_version": 4, "to_version": 5,
			"added":   map[string]any{"type": "FeatureCollection", "features": []json.RawMessage{prohibited("TZP003").json()}},
			"changed": map[string]any{"type": "FeatureCollection", "features": []json.RawMessage{changed.json()}},
			"removed": []string{"TZP001"}}
		for k, v := range m {
			base[k] = v
		}
		b, _ := json.Marshal(base)
		return b
	}
	body, to, err := mergeDelta(cur, held, delta(nil))
	if err != nil || to != 5 {
		t.Fatalf("merge: %v", err)
	}
	v, rf := ParseVersion(Zones, body, "", 5)
	if rf != nil || len(v.Collection.Features) != 2 || v.Collection.Features[0].Properties.Identifier != "TZP002" ||
		v.Collection.Features[1].Properties.Identifier != "TZP003" {
		t.Fatalf("merged: %v %s", rf, body)
	}
	empty := map[string]any{"type": "FeatureCollection", "features": []any{}}
	for name, m := range map[string]map[string]any{
		"dataset":   {"dataset": "restrictions"},
		"from":      {"from_version": 3},
		"to":        {"to_version": 4},
		"removed":   {"removed": []string{"TZX"}, "changed": empty, "added": empty},
		"changed":   {"removed": []string{}, "changed": map[string]any{"type": "FeatureCollection", "features": []json.RawMessage{prohibited("TZX").json()}}, "added": empty},
		"added":     {"removed": []string{}, "changed": empty, "added": map[string]any{"type": "FeatureCollection", "features": []json.RawMessage{prohibited("TZP001").json()}}},
		"no id":     {"removed": []string{}, "changed": map[string]any{"type": "FeatureCollection", "features": []any{map[string]any{}}}, "added": empty},
		"no id add": {"removed": []string{}, "changed": empty, "added": map[string]any{"type": "FeatureCollection", "features": []any{map[string]any{}}}},
	} {
		if _, _, err := mergeDelta(cur, held, delta(m)); !errors.Is(err, errDeltaUnusable) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, _, err := mergeDelta(cur, held, []byte("[]")); !errors.Is(err, errDeltaUnusable) {
		t.Fatalf("not an object: %v", err)
	}
	if _, _, err := mergeDelta(cur, held, []byte(`{"added":[]}`)); !errors.Is(err, errDeltaUnusable) {
		t.Fatalf("added not a collection: %v", err)
	}
	// Removed and added again in one delta: listed once.
	body, _, err = mergeDelta(cur, held, delta(map[string]any{"changed": empty,
		"added": map[string]any{"type": "FeatureCollection", "features": []json.RawMessage{prohibited("TZP001").json()}}}))
	if err != nil || strings.Count(string(body), `"TZP001"`) != 1 {
		t.Fatalf("re-added: %v %s", err, body)
	}
}
