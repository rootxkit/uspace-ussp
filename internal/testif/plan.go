package testif

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/national/client"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	fp "github.com/rootxkit/uspace-ussp/internal/stdapi/interuss/flightplanning"
)

// The flight_planning v1 API's name and version, as the pinned file says
// StatusResponse must carry them ("Must be ... when implementing this
// version of the API"; TestStatusNamesThePinnedVersion reads the file).
const (
	fpAPIName    = "Flight Planning Automated Testing Interface"
	fpAPIVersion = "v0.7.0"
)

// The intent states and decisions of intent/decision/v1 and the actions
// of IntentPatch (api/openapi.yaml), as the USSP answers them;
// TestPlanTableCoversEveryDecision holds them to the schema and the
// contract.
const (
	actionActivate = "activate"
	actionModify   = "modify"
	actionEnd      = "end"

	stateAccepted          = "accepted"
	stateActivated         = "activated"
	stateNonconforming     = "nonconforming"
	stateContingent        = "contingent"
	stateEnded             = "ended"
	stateRejected          = "rejected"
	stateWithdrawn         = "withdrawn"
	statePendingValidation = "pending_validation"
	statePendingDSS        = "pending_dss"
	statePendingAuthority  = "pending_authority"
)

// Counters of flight_planning (E-09).
const (
	CounterPlanCompleted    = "testif_plan_completed"
	CounterPlanRejected     = "testif_plan_rejected"
	CounterPlanFailed       = "testif_plan_failed"
	CounterPlanNotSupported = "testif_plan_not_supported"
	CounterPlanReplayed     = "testif_plan_request_replayed"
	CounterPlanForgotten    = "testif_plan_forgotten"
	CounterAreaCleared      = "testif_area_cleared"
)

// planRow is one row of the decision table: the result and status a
// state of the intent the USSP decided maps onto (brief WP-21: every
// Rejected row has its Completed twin).
type planRow struct {
	result fp.PlanningActivityResult
	status fp.FlightPlanStatus
	notes  string
}

// planTable maps the intent's state after the USSP's decision onto the
// InterUSS result. The decision is the intent service's; nothing here
// judges: pending_dss is Failed (the suite must see that the DSS was not
// written), pending_validation and pending_authority are Failed naming
// the state, rejected is Rejected with the conflicts.
var planTable = map[string]planRow{
	stateAccepted:          {fp.PlanningActivityResultCompleted, fp.FlightPlanStatusPlanned, ""},
	stateActivated:         {fp.PlanningActivityResultCompleted, fp.FlightPlanStatusOkToFly, ""},
	stateNonconforming:     {fp.PlanningActivityResultCompleted, fp.FlightPlanStatusOffNominal, ""},
	stateContingent:        {fp.PlanningActivityResultCompleted, fp.FlightPlanStatusOffNominal, ""},
	stateEnded:             {fp.PlanningActivityResultCompleted, fp.FlightPlanStatusClosed, ""},
	stateWithdrawn:         {fp.PlanningActivityResultCompleted, fp.FlightPlanStatusClosed, "the USSP withdrew the authorisation"},
	stateRejected:          {fp.PlanningActivityResultRejected, fp.FlightPlanStatusNotPlanned, ""},
	statePendingDSS:        {fp.PlanningActivityResultFailed, fp.FlightPlanStatusNotPlanned, "pending_dss: the DSS was not written"},
	statePendingValidation: {fp.PlanningActivityResultFailed, fp.FlightPlanStatusNotPlanned, "pending_validation: the registry has not validated the request"},
	statePendingAuthority:  {fp.PlanningActivityResultFailed, fp.FlightPlanStatusNotPlanned, "pending_authority: the authority's approval is pending"},
}

// terminal states: a new upsert of the plan files a new intent.
var terminal = []string{stateEnded, stateRejected, stateWithdrawn}

// openStates are those clear_area ends.
func open(state string) bool { return !slices.Contains(terminal, state) }

// outcome is what an upsert or a delete answers.
type outcome struct {
	result fp.PlanningActivityResult
	status fp.FlightPlanStatus
	notes  []string
}

func (o outcome) upsert() fp.UpsertFlightPlanResponse {
	var r fp.UpsertFlightPlanResponse
	_ = r.PlanningResult.FromPlanningActivityResult(o.result)
	_ = r.FlightPlanStatus.FromFlightPlanStatus(o.status)
	if len(o.notes) > 0 {
		n := clip(strings.Join(o.notes, "; "), 2048)
		r.Notes = &n
	}
	return r
}

func (o outcome) delete() fp.DeleteFlightPlanResponse {
	var r fp.DeleteFlightPlanResponse
	_ = r.PlanningResult.FromPlanningActivityResult(o.result)
	_ = r.FlightPlanStatus.FromFlightPlanStatus(o.status)
	if len(o.notes) > 0 {
		n := clip(strings.Join(o.notes, "; "), 2048)
		r.Notes = &n
	}
	return r
}

// stateOf is the state of a decision as the USSP wrote it.
func stateOf(d *client.IntentDecision) string { s, _ := d.State.(string); return s }

// outcomeOfDecision maps the USSP's decision onto the InterUSS result
// through planTable; a state the table does not name is Failed, said so.
func outcomeOfDecision(d *client.IntentDecision) outcome {
	st := stateOf(d)
	row, ok := planTable[st]
	if !ok {
		return outcome{fp.PlanningActivityResultFailed, fp.FlightPlanStatusNotPlanned, []string{"the USSP answered state " + strconv.Quote(clip(st, 32)) + ", which this interface does not map"}}
	}
	o := outcome{result: row.result, status: row.status}
	if row.notes != "" {
		o.notes = append(o.notes, row.notes)
	}
	for _, c := range d.Conflicts {
		kind, _ := c.Kind.(string)
		o.notes = append(o.notes, clip(fmt.Sprintf("%s %s: %s", kind, c.Reason, c.Detail), 256))
	}
	return o
}

// outcomeOfRefusal maps a refusal of the request (no decision) onto the
// result: a 4xx refusal of what the user asked (a validation, an
// unbound serial, an operator that is not the client's, a transition the
// state does not allow) is Rejected; the USSP unreachable, 429 or 5xx is
// Failed.
func outcomeOfRefusal(err error, status fp.FlightPlanStatus) outcome {
	var ref *RefusalError
	if errors.As(err, &ref) && ref.Status >= 400 && ref.Status < 500 && ref.Status != http.StatusTooManyRequests {
		return outcome{fp.PlanningActivityResultRejected, status, []string{"the USSP refused the request: " + ref.Error()}}
	}
	return outcome{fp.PlanningActivityResultFailed, status, []string{"the USSP could not decide: " + clip(err.Error(), 512)}}
}

// planRec is what the planner remembers of a flight plan: the intent it
// filed and the attempt (a plan upserted again after a terminal state
// files a new intent under the next client_ref).
type planRec struct {
	id       string
	intentID string
	attempt  int
	volumes  json.RawMessage
	state    string
}

// clientRefPrefix is the client_ref of a plan's intents: "fp:<id>:<n>".
func clientRefPrefix(id string) string { return "fp:" + id + ":" }

// planner is the flight_planning v1 interface.
type planner struct {
	s *Server

	mu       sync.Mutex
	plans    map[string]*list.Element // flight plan id -> *planRec
	order    *list.List
	requests map[string]*list.Element // request_id -> requestRec
	reqOrder *list.List
	locks    map[string]*planLock
}

type requestRec struct {
	id   string
	resp fp.UpsertFlightPlanResponse
}

// planLock serialises the requests of one plan.
type planLock struct {
	mu   sync.Mutex
	refs int
}

func newPlanner(s *Server) *planner {
	return &planner{s: s, plans: map[string]*list.Element{}, order: list.New(), requests: map[string]*list.Element{},
		reqOrder: list.New(), locks: map[string]*planLock{}}
}

var _ fp.StrictServerInterface = (*planner)(nil)

func (p *planner) iface() iface {
	return iface{name: "flight_planning", access: FlightPlanningAccess(), register: func(g *httpx.GuardedMux) {
		h := fp.NewStrictHandlerWithOptions(p, nil, fp.StrictHTTPServerOptions{
			RequestErrorHandlerFunc: p.requestError, ResponseErrorHandlerFunc: p.responseError,
		})
		fp.HandlerWithOptions(h, fp.StdHTTPServerOptions{BaseURL: BaseFlightPlanning, BaseRouter: g, ErrorHandlerFunc: p.requestError})
	}}
}

func (p *planner) requestError(w http.ResponseWriter, r *http.Request, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.NewProblem(http.StatusBadRequest, httpx.SlugValidation, "", "the request is not the operation's", core.Fieldf("request", "malformed")).Write(w, r)
}

func (p *planner) responseError(w http.ResponseWriter, r *http.Request, err error) {
	p.s.Logger.Error("flight_planning handler failed", "error", err.Error())
	httpx.NewProblem(http.StatusInternalServerError, httpx.SlugInternal, "", "").Write(w, r)
}

func (p *planner) count(o outcome) {
	switch o.result {
	case fp.PlanningActivityResultCompleted:
		p.s.Counters.Inc(CounterPlanCompleted)
	case fp.PlanningActivityResultRejected:
		p.s.Counters.Inc(CounterPlanRejected)
	case fp.PlanningActivityResultNotSupported:
		p.s.Counters.Inc(CounterPlanNotSupported)
	case fp.PlanningActivityResultFailed:
		p.s.Counters.Inc(CounterPlanFailed)
	default:
		p.s.Counters.Inc(CounterPlanFailed)
	}
}

// lock serialises one plan's requests; the returned function unlocks.
func (p *planner) lock(id string) func() {
	p.mu.Lock()
	l := p.locks[id]
	if l == nil {
		l = &planLock{}
		p.locks[id] = l
	}
	l.refs++
	p.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		p.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(p.locks, id)
		}
		p.mu.Unlock()
	}
}

// remember puts rec first, forgetting the oldest beyond the bound
// (USSP_TEST_INTERFACES_MAX_PLANS; a forgotten plan is found again
// through GET /v1/intents).
func (p *planner) remember(rec *planRec) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if el, ok := p.plans[rec.id]; ok {
		el.Value = rec
		p.order.MoveToFront(el)
		return
	}
	p.plans[rec.id] = p.order.PushFront(rec)
	for p.order.Len() > p.s.cfg.MaxPlans {
		old := p.order.Back()
		p.order.Remove(old)
		delete(p.plans, old.Value.(*planRec).id)
		p.s.Counters.Inc(CounterPlanForgotten)
	}
}

func (p *planner) recall(id string) *planRec {
	p.mu.Lock()
	defer p.mu.Unlock()
	if el, ok := p.plans[id]; ok {
		p.order.MoveToFront(el)
		c := *el.Value.(*planRec)
		return &c
	}
	return nil
}

func (p *planner) replay(requestID string) (fp.UpsertFlightPlanResponse, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if el, ok := p.requests[requestID]; ok {
		return el.Value.(requestRec).resp, true
	}
	return fp.UpsertFlightPlanResponse{}, false
}

func (p *planner) record(requestID string, resp fp.UpsertFlightPlanResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests[requestID] = p.reqOrder.PushFront(requestRec{id: requestID, resp: resp})
	for p.reqOrder.Len() > p.s.cfg.MaxPlans {
		old := p.reqOrder.Back()
		p.reqOrder.Remove(old)
		delete(p.requests, old.Value.(requestRec).id)
	}
}

// find is the plan's record: remembered, or the USSP's own record of its
// newest intent under the plan's client_ref (after a restart or once
// forgotten: restart persistence through the USSP, not here).
func (p *planner) find(ctx context.Context, c *client.ClientWithResponses, id string) (*planRec, error) {
	if rec := p.recall(id); rec != nil {
		return rec, nil
	}
	limit := 500
	resp, err := c.ListIntentsWithResponse(ctx, &client.ListIntentsParams{Limit: &limit})
	if err != nil {
		return nil, fmt.Errorf("GET /v1/intents: the USSP did not answer: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, problemOf(resp.StatusCode(), resp.Body)
	}
	var best *planRec
	prefix := clientRefPrefix(id)
	for i := range resp.JSON200.Intents {
		d := &resp.JSON200.Intents[i]
		if !strings.HasPrefix(d.ClientRef, prefix) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(d.ClientRef, prefix))
		if err != nil {
			continue
		}
		if best == nil || n > best.attempt {
			best = &planRec{id: id, intentID: d.IntentId, attempt: n, state: stateOf(d)}
		}
	}
	return best, nil
}

// GetStatus is Ready while the USSP's api answers /readyz 200, Starting
// otherwise (the file's two values).
func (p *planner) GetStatus(ctx context.Context, _ fp.GetStatusRequestObject) (fp.GetStatusResponseObject, error) {
	st := fp.StatusResponseStatusStarting
	if state, _ := p.s.USSP.Probe(ctx); state == obs.StateUp {
		st = fp.StatusResponseStatusReady
	}
	name, version := fpAPIName, fpAPIVersion
	return fp.GetStatus200JSONResponse{Status: st, ApiName: &name, ApiVersion: &version}, nil
}

// QueryUserNotifications answers the empty list: this USSP shows the
// virtual user no notification beside the decision.
func (p *planner) QueryUserNotifications(context.Context, fp.QueryUserNotificationsRequestObject) (fp.QueryUserNotificationsResponseObject, error) {
	return fp.QueryUserNotifications200JSONResponse{UserNotifications: []fp.UserNotification{}}, nil
}

// UpsertFlightPlan files or changes the plan's intent on the real path
// (POST /v1/intents, PATCH modify, activate, end) and answers the
// decision the intent service made.
func (p *planner) UpsertFlightPlan(ctx context.Context, req fp.UpsertFlightPlanRequestObject) (fp.UpsertFlightPlanResponseObject, error) {
	if req.Body == nil {
		return nil, errors.New("no body")
	}
	if resp, ok := p.replay(req.Body.RequestId); ok {
		p.s.Counters.Inc(CounterPlanReplayed)
		return fp.UpsertFlightPlan200JSONResponse(resp), nil
	}
	unlock := p.lock(req.FlightPlanId)
	defer unlock()
	o := p.upsert(ctx, req.FlightPlanId, req.Body)
	p.count(o)
	resp := o.upsert()
	p.record(req.Body.RequestId, resp)
	return fp.UpsertFlightPlan200JSONResponse(resp), nil
}

func (p *planner) upsert(ctx context.Context, id string, body *fp.UpsertFlightPlanJSONRequestBody) outcome {
	plan, err := body.FlightPlan.AsFlightPlan()
	if err != nil {
		return outcome{fp.PlanningActivityResultRejected, fp.FlightPlanStatusNotPlanned, []string{"flight_plan is not a FlightPlan"}}
	}
	style, err := body.ExecutionStyle.AsExecutionStyle()
	if err != nil || !style.Valid() {
		return outcome{fp.PlanningActivityResultRejected, fp.FlightPlanStatusNotPlanned, []string{"execution_style is not one of the file's"}}
	}
	if style == fp.ExecutionStyleHypothetical {
		return outcome{fp.PlanningActivityResultNotSupported, fp.FlightPlanStatusNotPlanned,
			[]string{"the USSP's API has no hypothetical planning: an operator files or does not"}}
	}
	c, err := p.s.USSP.Operator(ctx)
	if err != nil {
		return outcomeOfRefusal(err, fp.FlightPlanStatusNotPlanned)
	}
	rec, err := p.find(ctx, c, id)
	if err != nil {
		return outcomeOfRefusal(err, fp.FlightPlanStatusNotPlanned)
	}
	if plan.BasicInformation.UsageState == fp.BasicFlightPlanInformationUsageStateClosed {
		return p.close(ctx, c, rec)
	}
	var notes []string
	if plan.BasicInformation.UasState != nil {
		if us, err := plan.BasicInformation.UasState.AsFunctionalState(); err == nil &&
			(us == fp.FunctionalStateContingent || us == fp.FunctionalStateOffNominal) {
			notes = append(notes, "uas_state "+string(us)+" was not taken from the plan: the USSP's conformance monitor judges the state from telemetry")
		}
	}
	volumes, err := json.Marshal(derefArea(plan.BasicInformation.Area))
	if err != nil {
		return outcomeOfRefusal(err, fp.FlightPlanStatusNotPlanned)
	}
	var d *client.IntentDecision
	switch {
	case rec != nil && open(rec.state):
		d = nil
		if !jsonEqual(rec.volumes, volumes) {
			d, err = p.patch(ctx, c, rec.intentID, actionModify, volumes)
			if err != nil {
				return withNotes(outcomeOfRefusal(err, statusOfState(rec.state)), notes)
			}
			rec.volumes, rec.state = volumes, stateOf(d)
			p.remember(rec)
		}
	default:
		attempt := 1
		if rec != nil {
			attempt = rec.attempt + 1
		}
		body, err := p.request(plan, clientRefPrefix(id)+strconv.Itoa(attempt), volumes)
		if err != nil {
			return withNotes(outcomeOfRefusal(err, fp.FlightPlanStatusNotPlanned), notes)
		}
		d, err = p.create(ctx, c, body)
		if err != nil {
			return withNotes(outcomeOfRefusal(err, fp.FlightPlanStatusNotPlanned), notes)
		}
		rec = &planRec{id: id, intentID: d.IntentId, attempt: attempt, volumes: volumes, state: stateOf(d)}
		p.remember(rec)
	}
	if plan.BasicInformation.UsageState == fp.BasicFlightPlanInformationUsageStateInUse && rec.state == stateAccepted {
		ad, err := p.patch(ctx, c, rec.intentID, actionActivate, nil)
		if err != nil {
			return withNotes(outcomeOfRefusal(err, statusOfState(rec.state)), notes)
		}
		d = ad
		rec.state = stateOf(d)
		p.remember(rec)
	}
	if d == nil { // nothing changed: the intent as it stands
		gd, err := p.get(ctx, c, rec.intentID)
		if err != nil {
			return withNotes(outcomeOfRefusal(err, statusOfState(rec.state)), notes)
		}
		d = gd
	}
	return withNotes(outcomeOfDecision(d), notes)
}

func withNotes(o outcome, notes []string) outcome {
	o.notes = append(o.notes, notes...)
	return o
}

// statusOfState is the plan status an intent in state stands for.
func statusOfState(state string) fp.FlightPlanStatus {
	if row, ok := planTable[state]; ok {
		return row.status
	}
	return fp.FlightPlanStatusNotPlanned
}

func derefArea(a *[]fp.Volume4D) []fp.Volume4D {
	if a == nil {
		return []fp.Volume4D{}
	}
	return *a
}

func jsonEqual(a, b json.RawMessage) bool { return len(a) > 0 && string(a) == string(b) }

// intentBody is intent/request/v1 with the volumes carried as the plan's
// own JSON: the generated client's IntentVolume4D holds coordinates as
// float32, which would move a vertex by up to a few decimetres; the
// pinned file's Volume4D is the same F3548 shape with float64.
type intentBody struct {
	client.IntentRequest
	Volumes json.RawMessage `json:"volumes"`
}

type patchBody struct {
	Action  string          `json:"action"`
	Volumes json.RawMessage `json:"volumes,omitempty"`
}

func (p *planner) create(ctx context.Context, c *client.ClientWithResponses, body intentBody) (*client.IntentDecision, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := c.CreateIntentWithBodyWithResponse(ctx, "application/json", strings.NewReader(string(b)))
	if err != nil {
		return nil, fmt.Errorf("POST /v1/intents: the USSP did not answer: %w", err)
	}
	if d := firstDecision(resp.JSON201, resp.JSON200); d != nil {
		return d, nil
	}
	return nil, problemOf(resp.StatusCode(), resp.Body)
}

func (p *planner) patch(ctx context.Context, c *client.ClientWithResponses, intentID, action string, volumes json.RawMessage) (*client.IntentDecision, error) {
	var id client.IntentID
	if err := id.UnmarshalText([]byte(intentID)); err != nil {
		return nil, &RefusalError{Status: http.StatusNotFound, Reason: "not_found", Detail: "intent id " + clip(intentID, 40)}
	}
	b, err := json.Marshal(patchBody{Action: action, Volumes: volumes})
	if err != nil {
		return nil, err
	}
	resp, err := c.ChangeIntentWithBodyWithResponse(ctx, id, "application/json", strings.NewReader(string(b)))
	if err != nil {
		return nil, fmt.Errorf("PATCH /v1/intents/%s %s: the USSP did not answer: %w", intentID, action, err)
	}
	if resp.JSON200 != nil {
		return resp.JSON200, nil
	}
	return nil, problemOf(resp.StatusCode(), resp.Body)
}

func (p *planner) get(ctx context.Context, c *client.ClientWithResponses, intentID string) (*client.IntentDecision, error) {
	var id client.IntentID
	if err := id.UnmarshalText([]byte(intentID)); err != nil {
		return nil, &RefusalError{Status: http.StatusNotFound, Reason: "not_found", Detail: "intent id " + clip(intentID, 40)}
	}
	resp, err := c.GetIntentWithResponse(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("GET /v1/intents/%s: the USSP did not answer: %w", intentID, err)
	}
	if resp.JSON200 != nil {
		return resp.JSON200, nil
	}
	return nil, problemOf(resp.StatusCode(), resp.Body)
}

func firstDecision(ds ...*client.IntentDecision) *client.IntentDecision {
	for _, d := range ds {
		if d != nil {
			return d
		}
	}
	return nil
}

// close ends the plan's intent (PATCH end): Completed and Closed, or the
// USSP's refusal; a plan the USSP has no open intent for is Closed.
func (p *planner) close(ctx context.Context, c *client.ClientWithResponses, rec *planRec) outcome {
	if rec == nil || !open(rec.state) {
		return outcome{result: fp.PlanningActivityResultCompleted, status: fp.FlightPlanStatusClosed}
	}
	d, err := p.patch(ctx, c, rec.intentID, actionEnd, nil)
	if err != nil {
		return outcomeOfRefusal(err, statusOfState(rec.state))
	}
	rec.state = stateOf(d)
	p.remember(rec)
	return outcomeOfDecision(d)
}

// DeleteFlightPlan ends the plan's intent (the test director's close).
func (p *planner) DeleteFlightPlan(ctx context.Context, req fp.DeleteFlightPlanRequestObject) (fp.DeleteFlightPlanResponseObject, error) {
	unlock := p.lock(req.FlightPlanId)
	defer unlock()
	c, err := p.s.USSP.Operator(ctx)
	if err != nil {
		o := outcomeOfRefusal(err, fp.FlightPlanStatusNotPlanned)
		p.count(o)
		return fp.DeleteFlightPlan200JSONResponse(o.delete()), nil
	}
	rec, err := p.find(ctx, c, req.FlightPlanId)
	if err != nil {
		o := outcomeOfRefusal(err, fp.FlightPlanStatusNotPlanned)
		p.count(o)
		return fp.DeleteFlightPlan200JSONResponse(o.delete()), nil
	}
	if rec == nil {
		return fp.DeleteFlightPlan404Response{}, nil
	}
	o := p.close(ctx, c, rec)
	p.count(o)
	return fp.DeleteFlightPlan200JSONResponse(o.delete()), nil
}

// envelope is the conservative box and window of an F3548 Volume4D's
// JSON (the plan's area and the extent are that shape), by uspace-core.
func envelope(raw json.RawMessage) (geodesy.BBox, time.Time, time.Time, error) {
	var v f3548.Volume4D
	if err := json.Unmarshal(raw, &v); err != nil {
		return geodesy.BBox{}, time.Time{}, time.Time{}, core.Fieldf("volume", "not a Volume4D")
	}
	return f3548.Volume4DToZonesEnvelope(v)
}

// meet reports whether two envelopes share a point: the boxes overlap
// and the windows do (an absent bound is open). Conservative, as the
// boxes are: clear_area may end more than it must, never less.
func meet(a geodesy.BBox, as, ae time.Time, b geodesy.BBox, bs, be time.Time) bool {
	if a.MaxLat < b.MinLat || b.MaxLat < a.MinLat || a.MaxLon < b.MinLon || b.MaxLon < a.MinLon {
		return false
	}
	if !ae.IsZero() && !bs.IsZero() && ae.Before(bs) {
		return false
	}
	if !be.IsZero() && !as.IsZero() && be.Before(as) {
		return false
	}
	return true
}

// ClearArea ends every open intent of the test operator whose volumes
// meet the extent (GET /v1/intents, PATCH end): an intent whose volumes
// this process does not know (filed before a restart, or forgotten) is
// ended too, and said so. The outcome names what was ended and what
// could not be, with the USSP's reason.
func (p *planner) ClearArea(ctx context.Context, req fp.ClearAreaRequestObject) (fp.ClearAreaResponseObject, error) {
	if req.Body == nil {
		return nil, errors.New("no body")
	}
	fail := func(msg string) (fp.ClearAreaResponseObject, error) {
		no := false
		m := clip(msg, 1024)
		return fp.ClearArea200JSONResponse{Outcome: fp.ClearAreaOutcome{Success: &no, Message: &m}}, nil
	}
	ext, err := req.Body.Extent.AsVolume4D()
	if err != nil {
		return fail("extent is not a Volume4D")
	}
	rawExt, err := json.Marshal(ext)
	if err != nil {
		return nil, err
	}
	eb, es, ee, err := envelope(rawExt)
	if err != nil {
		return fail("extent: " + err.Error())
	}
	c, err := p.s.USSP.Operator(ctx)
	if err != nil {
		return fail(err.Error())
	}
	limit := 500
	resp, err := c.ListIntentsWithResponse(ctx, &client.ListIntentsParams{Limit: &limit})
	if err != nil {
		return fail("GET /v1/intents: the USSP did not answer")
	}
	if resp.JSON200 == nil {
		return fail("GET /v1/intents: " + problemOf(resp.StatusCode(), resp.Body).Error())
	}
	known := map[string]json.RawMessage{}
	p.mu.Lock()
	for _, el := range p.plans {
		rec := el.Value.(*planRec)
		known[rec.intentID] = rec.volumes
	}
	p.mu.Unlock()
	var ended, unknown, failed []string
	for i := range resp.JSON200.Intents {
		d := &resp.JSON200.Intents[i]
		if !open(stateOf(d)) {
			continue
		}
		vols, ok := known[d.IntentId]
		if ok && !p.meets(vols, eb, es, ee) {
			continue
		}
		if _, err := p.patch(ctx, c, d.IntentId, actionEnd, nil); err != nil {
			failed = append(failed, d.IntentId+": "+err.Error())
			continue
		}
		ended = append(ended, d.IntentId)
		if !ok {
			unknown = append(unknown, d.IntentId)
		}
	}
	p.s.Counters.Inc(CounterAreaCleared)
	success := len(failed) == 0
	details := map[string]any{"ended": nonNil(ended), "ended_volumes_unknown": nonNil(unknown), "not_ended": nonNil(failed)}
	msg := fmt.Sprintf("%d intents ended, %d could not be", len(ended), len(failed))
	return fp.ClearArea200JSONResponse{Outcome: fp.ClearAreaOutcome{Success: &success, Message: &msg, Details: &details}}, nil
}

// meets reports whether any volume of an intent's JSON meets the
// extent; volumes it cannot bound meet it (conservative).
func (p *planner) meets(vols json.RawMessage, eb geodesy.BBox, es, ee time.Time) bool {
	var list []json.RawMessage
	if json.Unmarshal(vols, &list) != nil || len(list) == 0 {
		return true
	}
	for _, v := range list {
		b, s, e, err := envelope(v)
		if err != nil || meet(b, s, e, eb, es, ee) {
			return true
		}
	}
	return false
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
