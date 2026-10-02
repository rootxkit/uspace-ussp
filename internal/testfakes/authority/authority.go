// Package authority is a fake authority for tests: the executable
// reading of spec 02 F8 that internal/registry is tested against. It
// serves the subset of the pinned api/clients/authority.yaml the cache
// calls (GET and POST /v1/registry/validate with the purpose query
// parameter, GET /v1/registry/changes with since, limit and ETag),
// answers status only as the authority's F8 does (an operator number
// echoed as its public part, an expired registration revoked) and
// records a change for every status it is given. Down makes every
// request answer 503; Misbehave adds a field F8 does not define to
// every lookup answer; Requests counts what was asked.
package authority

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/regnum"
	"github.com/rootxkit/uspace-core/serial"
)

// Token is the bearer the fake accepts.
const Token = "fake-authority-token"

// Tokens hands out Token for any audience and scope.
type Tokens struct{}

// Token implements registry.TokenSource.
func (Tokens) Token(context.Context, string, ...string) (string, error) { return Token, nil }

type operator struct {
	number     string
	status     string
	validUntil *time.Time
}

type uas struct {
	serial, status, classLabel, mtomBand string
}

// Competency is one competency of a fake pilot.
type Competency struct {
	Competency string    `json:"competency"`
	ValidUntil time.Time `json:"valid_until"`
}

type pilot struct {
	status       string
	competencies []Competency
}

// Change is one record of the fake's change feed.
type Change struct {
	Seq        int64     `json:"seq"`
	EntityType string    `json:"entity_type"`
	EntityID   string    `json:"entity_id"`
	PublicKey  string    `json:"public_key"`
	Status     string    `json:"status"`
	At         time.Time `json:"at"`
}

// Fake is a running fake authority.
type Fake struct {
	srv *httptest.Server

	mu        sync.Mutex
	bearer    string
	down      bool
	extra     map[string]any
	echoAs    string
	operators map[string]operator // by regnum.CompareKey
	uas       map[string]uas      // by serial as registered
	pilots    map[string]pilot
	changes   []Change
	requests  map[string]int
	purposes  []string
}

// New starts a fake.
func New() *Fake {
	f := &Fake{bearer: Token, operators: map[string]operator{}, uas: map[string]uas{}, pilots: map[string]pilot{}, requests: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Close stops the fake.
func (f *Fake) Close() { f.srv.Close() }

// URL is the fake's base URL.
func (f *Fake) URL() string { return f.srv.URL }

// AcceptBearer makes the fake accept tok instead of Token (a test whose
// token service hands out another bearer).
func (f *Fake) AcceptBearer(tok string) { f.mu.Lock(); f.bearer = tok; f.mu.Unlock() }

// Down makes every request answer 503 until Up.
func (f *Fake) Down() { f.mu.Lock(); f.down = true; f.mu.Unlock() }

// Up ends Down.
func (f *Fake) Up() { f.mu.Lock(); f.down = false; f.mu.Unlock() }

// Misbehave adds field: value to every lookup answer's parts (nil
// removes every added field): an authority that sends what F8 does not
// define, such as a name.
func (f *Fake) Misbehave(field string, value any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if value == nil {
		f.extra = nil
		return
	}
	if f.extra == nil {
		f.extra = map[string]any{}
	}
	f.extra[field] = value
}

// EchoOperatorAs makes every operator answer echo number instead of the
// public part asked ("" restores the echo).
func (f *Fake) EchoOperatorAs(number string) { f.mu.Lock(); f.echoAs = number; f.mu.Unlock() }

// Requests counts the requests of an operation: "validate" (GET),
// "validate_batch" (POST) and "changes".
func (f *Fake) Requests(op string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.requests[op] }

// Purposes are the purposes the lookups carried, in order.
func (f *Fake) Purposes() []string { f.mu.Lock(); defer f.mu.Unlock(); return slices.Clone(f.purposes) }

func (f *Fake) record(entity, key, status string) {
	f.changes = append(f.changes, Change{Seq: int64(len(f.changes) + 1), EntityType: entity, EntityID: entity + "-" + key,
		PublicKey: key, Status: status, At: time.Now().UTC()})
}

// SetOperator registers an operator with a registry status (active,
// suspended, revoked, expired) and records the change.
func (f *Fake) SetOperator(number, status string, validUntil *time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.operators[regnum.CompareKey(number)] = operator{number: regnum.PublicPart(number), status: status, validUntil: validUntil}
	f.record("operator", regnum.PublicPart(number), status)
}

// SetUAS registers an aircraft and records the change.
func (f *Fake) SetUAS(sn, status, classLabel, mtomBand string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uas[sn] = uas{serial: sn, status: status, classLabel: classLabel, mtomBand: mtomBand}
	f.record("uas", sn, status)
}

// SetPilot registers a remote pilot and records the change.
func (f *Fake) SetPilot(id, status string, competencies ...Competency) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pilots[id] = pilot{status: status, competencies: competencies}
	f.record("pilot", id, status)
}

// Changes is the fake's feed.
func (f *Fake) Changes() []Change { f.mu.Lock(); defer f.mu.Unlock(); return slices.Clone(f.changes) }

func problem(w http.ResponseWriter, status int, slug string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "https://schemas.uspace.ge/problems/" + slug, "title": slug, "status": status, "errors": []any{}})
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		problem(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.bearer {
		problem(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	switch {
	case r.URL.Path == "/v1/registry/validate" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		f.validate(w, r)
	case r.URL.Path == "/v1/registry/changes" && r.Method == http.MethodGet:
		f.listChanges(w, r)
	default:
		problem(w, http.StatusNotFound, "not_found")
	}
}

type item struct {
	Operator string `json:"operator"`
	Serial   string `json:"serial"`
	Pilot    string `json:"pilot"`
}

func (f *Fake) validate(w http.ResponseWriter, r *http.Request) {
	purpose := r.URL.Query().Get("purpose")
	if purpose != "authorisation" && purpose != "identification" {
		problem(w, http.StatusBadRequest, "validation")
		return
	}
	f.purposes = append(f.purposes, purpose)
	if r.Method == http.MethodGet {
		f.requests["validate"]++
		q := r.URL.Query()
		writeJSON(w, f.answer(item{Operator: q.Get("operator"), Serial: q.Get("serial"), Pilot: q.Get("pilot")}))
		return
	}
	f.requests["validate_batch"]++
	var body struct {
		Items []item `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Items) == 0 || len(body.Items) > 100 {
		problem(w, http.StatusBadRequest, "validation")
		return
	}
	out := make([]map[string]any, 0, len(body.Items))
	for _, it := range body.Items {
		out = append(out, f.answer(it))
	}
	writeJSON(w, map[string]any{"results": out})
}

// validity is the F8 value of a registry status.
func validity(status string) string {
	switch status {
	case "active":
		return "valid"
	case "suspended":
		return "suspended"
	case "revoked", "expired":
		return "revoked"
	}
	return "unknown"
}

func (f *Fake) part(m map[string]any) map[string]any {
	for k, v := range f.extra {
		m[k] = v
	}
	return m
}

func (f *Fake) answer(it item) map[string]any {
	out := map[string]any{}
	if it.Operator != "" {
		echo := regnum.PublicPart(it.Operator)
		if f.echoAs != "" {
			echo = f.echoAs
		}
		m := map[string]any{"registration_number": echo, "status": "unknown"}
		if o, ok := f.operators[regnum.CompareKey(it.Operator)]; ok {
			m["status"] = validity(o.status)
			if o.validUntil != nil {
				m["valid_until"] = o.validUntil.UTC().Format(time.RFC3339)
			}
		}
		out["operator"] = f.part(m)
	}
	if it.Serial != "" {
		m := map[string]any{"serial": it.Serial, "status": "unknown"}
		u, ok := f.uas[it.Serial]
		if !ok {
			var found []uas
			for _, c := range f.uas {
				if serial.FoldKey(c.serial) == serial.FoldKey(it.Serial) {
					found = append(found, c)
				}
			}
			if len(found) == 1 {
				u, ok = found[0], true
			}
		}
		if ok {
			m["status"] = validity(u.status)
			if u.classLabel != "" {
				m["class_label"] = u.classLabel
			}
			if u.mtomBand != "" {
				m["mtom_band"] = u.mtomBand
			}
		}
		out["uas"] = f.part(m)
	}
	if it.Pilot != "" {
		m := map[string]any{"pilot": it.Pilot, "status": "unknown", "competencies": []Competency{}}
		if p, ok := f.pilots[it.Pilot]; ok {
			m["status"] = validity(p.status)
			if p.competencies != nil {
				m["competencies"] = p.competencies
			}
		}
		out["pilot"] = f.part(m)
	}
	return out
}

func (f *Fake) listChanges(w http.ResponseWriter, r *http.Request) {
	f.requests["changes"]++
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 || limit > 1000 {
		limit = 500
	}
	var page []Change
	for _, c := range f.changes {
		if c.Seq > since && len(page) < limit {
			page = append(page, c)
		}
	}
	next := since
	if len(page) > 0 {
		next = page[len(page)-1].Seq
	}
	etag := fmt.Sprintf(`"%d-%d"`, since, next)
	w.Header().Set("ETag", etag)
	if len(page) == 0 && strings.TrimSpace(r.Header.Get("If-None-Match")) == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if page == nil {
		page = []Change{}
	}
	writeJSON(w, map[string]any{"changes": page, "next_since": next})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
