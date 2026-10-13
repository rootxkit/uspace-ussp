package testif

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/national/client"
)

// fakeIntents is the intent API of the fake USSP: a decision per
// request by the switches below, as the intent service would answer it.
type fakeIntents struct {
	mu      sync.Mutex
	byID    map[string]*fakeIntent
	order   []string
	posts   []json.RawMessage
	patches []string
	dssDown bool
	n       int
}

type fakeIntent struct {
	d       client.IntentDecision
	volumes json.RawMessage
}

func newFakeIntents() *fakeIntents { return &fakeIntents{byID: map[string]*fakeIntent{}} }

// decide is the fake intent service: an operator other than the test
// operator's is 403, a serial "BAD" 400 annex_iv_invalid, a serial
// containing CONFLICT rejected with an intent conflict, the DSS down
// pending_dss, otherwise authorised (accepted).
func (fi *fakeIntents) decide(serial, operator string) (int, string, []client.IntentConflict, string) {
	switch {
	case operator != "GEO-TEST-WP21-0001":
		return http.StatusForbidden, "", nil, "operator_mismatch"
	case serial == "BAD":
		return http.StatusBadRequest, "", nil, "annex_iv_invalid"
	case strings.Contains(serial, "CONFLICT"):
		item := 5
		return http.StatusCreated, stateRejected, []client.IntentConflict{{Kind: "intent", Reason: "higher_priority_intent", Effect: "rejects",
			Detail: "intersects an authorised intent of priority 100", Volume: &item}}, ""
	case fi.dssDown:
		return http.StatusCreated, statePendingDSS, nil, ""
	}
	return http.StatusCreated, stateAccepted, nil, ""
}

func decisionOf(state string) string {
	switch state {
	case stateAccepted, stateActivated:
		return "authorised"
	case stateRejected:
		return "rejected"
	}
	return state
}

func (fi *fakeIntents) routes(mux *http.ServeMux) {
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			problem(w, http.StatusUnauthorized, "unauthenticated", "no token")
			return false
		}
		return true
	}
	mux.HandleFunc("POST /v1/intents", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		var req struct {
			ClientRef   string          `json:"client_ref"`
			UasSerial   string          `json:"uas_serial"`
			OperatorReg string          `json:"operator_reg"`
			Volumes     json.RawMessage `json:"volumes"`
		}
		_ = json.Unmarshal(raw, &req)
		fi.mu.Lock()
		defer fi.mu.Unlock()
		fi.posts = append(fi.posts, raw)
		for _, id := range fi.order {
			if fi.byID[id].d.ClientRef == req.ClientRef {
				problem(w, http.StatusConflict, "client_ref_reused", "another body under this client_ref")
				return
			}
		}
		status, state, conflicts, slug := fi.decide(req.UasSerial, req.OperatorReg)
		if slug != "" {
			problem(w, status, slug, "refused: "+slug)
			return
		}
		fi.n++
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", fi.n)
		in := &fakeIntent{volumes: req.Volumes, d: client.IntentDecision{IntentId: id, ClientRef: req.ClientRef, State: state,
			Decision: decisionOf(state), Conflicts: conflicts, Version: 1, DecidedAt: time.Now().UTC()}}
		if in.d.Conflicts == nil {
			in.d.Conflicts = []client.IntentConflict{}
		}
		fi.byID[id] = in
		fi.order = append(fi.order, id)
		writeJSON(w, http.StatusCreated, in.d)
	})
	mux.HandleFunc("GET /v1/intents", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		fi.mu.Lock()
		defer fi.mu.Unlock()
		out := client.IntentList{Intents: []client.IntentDecision{}}
		for _, id := range slices.Backward(fi.order) {
			out.Intents = append(out.Intents, fi.byID[id].d)
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /v1/intents/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		fi.mu.Lock()
		defer fi.mu.Unlock()
		in, ok := fi.byID[r.PathValue("id")]
		if !ok {
			problem(w, http.StatusNotFound, "not_found", "no intent")
			return
		}
		writeJSON(w, http.StatusOK, in.d)
	})
	mux.HandleFunc("PATCH /v1/intents/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var p struct {
			Action  string          `json:"action"`
			Volumes json.RawMessage `json:"volumes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		fi.mu.Lock()
		defer fi.mu.Unlock()
		in, ok := fi.byID[r.PathValue("id")]
		if !ok {
			problem(w, http.StatusNotFound, "not_found", "no intent")
			return
		}
		fi.patches = append(fi.patches, p.Action+" "+r.PathValue("id"))
		st, _ := in.d.State.(string)
		switch p.Action {
		case actionActivate:
			if st != stateAccepted {
				problem(w, http.StatusConflict, "transition_not_allowed", "activate from "+st)
				return
			}
			in.d.State = stateActivated
		case actionModify:
			if len(p.Volumes) == 0 {
				problem(w, http.StatusBadRequest, "validation", "volumes required")
				return
			}
			in.volumes = p.Volumes
			in.d.Version++
			if fi.dssDown {
				in.d.State = statePendingDSS
			}
		case actionEnd:
			in.d.State = stateEnded
		default:
			problem(w, http.StatusBadRequest, "validation", "action")
			return
		}
		in.d.Decision = decisionOf(in.d.State.(string))
		writeJSON(w, http.StatusOK, in.d)
	})
}
