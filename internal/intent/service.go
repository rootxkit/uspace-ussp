package intent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/ed269"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/geoid"
	"github.com/rootxkit/uspace-core/serial"

	"github.com/rootxkit/uspace-ussp/internal/intent/deconflict"
	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// Bounds of the service (E-10).
const (
	// MaxOverlapping bounds the local intents one decision reads; more
	// refuses the request with 503 (docs/PLAN.md §9: at most 1000
	// active intents are expected).
	MaxOverlapping = deconflict.MaxOthers - MaxPeerIntents
	// MaxPeerIntents bounds the peers' intents one decision reads.
	MaxPeerIntents = 1000
	// MaxList bounds one page of GET /v1/intents.
	MaxList = 500
	// SweepBatch bounds the intents ended by one sweep.
	SweepBatch = 500
	// PeerMaxAge is how long a peer's intent is used (F3548
	// ExternalDataMaxRetentionTimeHours).
	PeerMaxAge = time.Duration(f3548.ExternalDataMaxRetentionTimeHours) * time.Hour
)

// Actions of PATCH /v1/intents/{id}.
const (
	ActionActivate = "activate"
	ActionModify   = "modify"
	ActionEnd      = "end"
)

// Event types of the audit log.
const (
	EventSubmitted = "intent_submitted"
	EventActivated = "intent_activated"
	EventModified  = "intent_modified"
	EventEnded     = "intent_ended"
	EventExpired   = "intent_expired"
	EventFlagged   = "intent_update_required"
	// PurposeAuthorisation is the purpose of every intent audit row.
	PurposeAuthorisation = "authorisation"
)

// ErrNotFound is an intent, a client or an operator that does not exist
// for the caller.
var ErrNotFound = errors.New("not found")

// ErrDuplicate is an insert that hit the (client_id, client_ref) index.
var ErrDuplicate = errors.New("duplicate client_ref")

// Error is a refusal of the service with its problem: status, slug,
// detail and, for a request that does not validate, the problems by
// field (every Annex IV problem names its item).
type Error struct {
	Status   int
	Slug     string
	Detail   string
	Problems *ed269.Problems
}

func (e *Error) Error() string { return e.Slug + ": " + e.Detail }

// HTTPStatus implements httpx.StatusError.
func (e *Error) HTTPStatus() int { return e.Status }

// ProblemSlug implements httpx.StatusError.
func (e *Error) ProblemSlug() string { return e.Slug }

// ProblemDetail implements httpx.StatusError.
func (e *Error) ProblemDetail() string { return e.Detail }

// FieldErrors are the problems as field errors (the problem body's
// errors[]).
func (e *Error) FieldErrors() []*core.FieldError {
	if e.Problems == nil {
		return nil
	}
	out := make([]*core.FieldError, len(e.Problems.List))
	for i, p := range e.Problems.List {
		out[i] = &core.FieldError{Field: p.Field, Reason: p.Reason}
	}
	return out
}

func refuse(status int, slug, format string, a ...any) *Error {
	return &Error{Status: status, Slug: slug, Detail: fmt.Sprintf(format, a...)}
}

// Owner is the operator account and client behind a caller.
type Owner struct {
	ClientID       string
	OperatorID     string
	OperatorKey    string
	OperatorStatus string
	ClientStatus   string
}

// Record is one operational intent as stored.
type Record struct {
	ID          string
	OperatorID  string
	ClientID    string
	ClientRef   string
	RequestHash string
	Request     Request
	Decision    Decision
	Version     int
	LocalState  string
	Exempt      bool
	Priority    int
	TimeStart   time.Time
	TimeEnd     time.Time
	// FiledAt is when the volumes judged were filed (first come, first
	// served), CreatedAt when the intent was.
	FiledAt   time.Time
	CreatedAt time.Time
	// VolumesAMSL and Cells are derived from Request.Volumes.
	VolumesAMSL []VolumeAMSL
	Cells       []string
	// Envelope is the volumes' boxes, padded for the store's prefilter.
	Envelope []geodesy.BBox
	// ChangeReason and Actor describe the version being written.
	ChangeReason string
	Actor        string
}

// PeerIntent is a peer USSP's intent the DSS told us about (WP-13
// fills peer_intents).
type PeerIntent struct {
	EntityID  string
	Priority  int
	FetchedAt time.Time
	Volumes   []f3548.Volume4D
}

// ListFilter is the query of GET /v1/intents.
type ListFilter struct {
	From, To *time.Time
	State    string
	Limit    int
}

// Store is the database side (pgstore implements it on PostgreSQL).
type Store interface {
	// Now is the database clock (expiry, horizons and ranks are judged
	// on it).
	Now(ctx context.Context) (time.Time, error)
	Owner(ctx context.Context, clientID string) (Owner, error)
	// SerialBound reports whether the client holds a live binding of
	// the serial's fold key.
	SerialBound(ctx context.Context, clientID, fold string) (bool, error)
	// ByClientRef is the intent of the client's reference, nil when
	// none.
	ByClientRef(ctx context.Context, clientID, ref string) (*Record, error)
	// Get is the intent, nil when none.
	Get(ctx context.Context, id string) (*Record, error)
	List(ctx context.Context, operatorID string, f ListFilter) ([]Record, error)
	// InTx runs fn in one transaction holding the intents lock: every
	// decision that reads the active intents and writes one is
	// serialised, so two overlapping requests are never both granted.
	InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// Tx is the store inside InTx.
type Tx interface {
	Now(ctx context.Context) (time.Time, error)
	// Overlapping are the active, non-exempt local intents whose
	// envelope is within distM of boxes and whose window overlaps
	// [from, to], without excludeID; at most limit+1 rows.
	Overlapping(ctx context.Context, boxes []geodesy.BBox, distM float64, from, to time.Time, excludeID string, limit int) ([]Record, error)
	// PeerIntents are the peers' intents fetched after since whose
	// window overlaps [from, to]; at most limit+1 rows.
	PeerIntents(ctx context.Context, since, from, to time.Time, limit int) ([]PeerIntent, error)
	CountOpen(ctx context.Context, operatorID string) (int, error)
	// Lock reads the intent FOR UPDATE, nil when none.
	Lock(ctx context.Context, id string) (*Record, error)
	// Insert writes the intent, its first version and its audit row;
	// ErrDuplicate when the client's reference is taken.
	Insert(ctx context.Context, r *Record) error
	// Update writes the intent's new version, its version row and its
	// audit row.
	Update(ctx context.Context, r *Record, event string) error
	// FlagUpdate records on each intent that by takes precedence over
	// it (Art. 10(10)) with its audit row.
	FlagUpdate(ctx context.Context, ids []string, by string, at time.Time) error
	// DueToEnd are the open intents whose time_end is before now, at
	// most limit, locked.
	DueToEnd(ctx context.Context, now time.Time, limit int) ([]Record, error)
}

// Projector writes an intent's state where the hot path and the other
// processes read it: the KV bucket intent_active (put for an active
// state, delete otherwise) and the subject intent.v1.<state>.<id>. It
// runs inside the transaction; an error rolls it back (B-09).
type Projector interface {
	Project(ctx context.Context, r *Record) error
}

// Service is the api's intent service: intake, decision, states.
type Service struct {
	Store     Store
	Decider   *Decider
	Geoid     geoid.Undulator
	Policy    func() policy.Record
	Projector Projector
	Counters  *core.Counters
	Logger    *slog.Logger
}

func (s *Service) count(name string) {
	if s.Counters != nil {
		s.Counters.Inc("intent_" + name)
	}
}

func (s *Service) logger() *slog.Logger {
	if s.Logger == nil {
		return obs.Discard()
	}
	return s.Logger
}

// newID is a random UUID (version 4): the intent id, which is also its
// DSS entity id.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// hashOf is the idempotency hash of a request: its canonical encoding.
func hashOf(r Request) string {
	b, _ := json.Marshal(r) // a decoded Request always encodes
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *Service) owner(ctx context.Context, clientID string) (Owner, error) {
	o, err := s.Store.Owner(ctx, clientID)
	if errors.Is(err, ErrNotFound) {
		return Owner{}, refuse(http.StatusForbidden, "client_unknown", "the client is not an operator client of this USSP")
	}
	if err != nil {
		return Owner{}, &UnavailableError{Dependency: "database", Detail: "the client's operator could not be read"}
	}
	if o.ClientStatus != "active" || o.OperatorStatus != "active" {
		return Owner{}, refuse(http.StatusForbidden, "operator_inactive", "the client or its operator account is not active")
	}
	return o, nil
}

func (s *Service) policy() policy.Record {
	if s.Policy == nil {
		return policy.Record{Values: policy.Defaults()}
	}
	return s.Policy()
}

// validate decodes nothing: it checks n's request and its ownership.
func (s *Service) validate(ctx context.Context, o Owner, r Request, pol policy.Record, now time.Time) (*Normalised, error) {
	n, probs, err := Validate(r, ValidateEnv{Geoid: s.Geoid, Now: now, SpecialPriority: pol.Values.SpecialOperationPriority})
	if err != nil {
		s.count("geoid_unavailable")
		return nil, err
	}
	if probs != nil {
		s.count("annex_iv_invalid")
		return nil, &Error{Status: http.StatusBadRequest, Slug: "annex_iv_invalid", Detail: "the request does not carry the Annex IV items in a form this USSP can judge", Problems: probs}
	}
	if n.OperatorKey != o.OperatorKey {
		s.count("operator_mismatch")
		return nil, refuse(http.StatusForbidden, "operator_mismatch", "operator_reg (annex_iv.10) is not the registration number of this client's operator")
	}
	bound, err := s.Store.SerialBound(ctx, o.ClientID, serial.FoldKey(n.Serial))
	if err != nil {
		return nil, &UnavailableError{Dependency: "database", Detail: "the client's serial bindings could not be read"}
	}
	if !bound {
		s.count("serial_not_bound")
		return nil, refuse(http.StatusForbidden, "serial_not_bound", "uas_serial (annex_iv.1) is not bound to this client")
	}
	return n, nil
}

// Submit is POST /v1/intents: the decision on a new request, created
// true; or, for a client_ref already used with the same body, the
// decision given then, created false (a different body under it is 409).
func (s *Service) Submit(ctx context.Context, clientID string, raw []byte) (Decision, bool, error) {
	req, err := Decode(raw)
	if err != nil {
		s.count("request_malformed")
		return Decision{}, false, err
	}
	o, err := s.owner(ctx, clientID)
	if err != nil {
		return Decision{}, false, err
	}
	hash := hashOf(req)
	if d, ok, err := s.replay(ctx, clientID, req.ClientRef, hash); ok || err != nil {
		return d, false, err
	}
	pol := s.policy()
	now, err := s.Store.Now(ctx)
	if err != nil {
		return Decision{}, false, &UnavailableError{Dependency: "database", Detail: "the database clock could not be read"}
	}
	n, err := s.validate(ctx, o, req, pol, now)
	if err != nil {
		return Decision{}, false, err
	}
	a := s.Decider.Assess(ctx, n, pol, now)
	id := newID()
	var out Decision
	err = s.Store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		open, err := tx.CountOpen(ctx, o.OperatorID)
		if err != nil {
			return err
		}
		if open >= pol.Values.IntentOpenMaxCount {
			s.count("open_bound_reached")
			return refuse(http.StatusTooManyRequests, "intent_bound_reached", "the operator holds %d open intents, the most the policy allows", open)
		}
		at, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		others, err := s.others(ctx, tx, n, id)
		if err != nil {
			return err
		}
		d, flagged := s.Decider.Finish(a, id, at, others)
		r := &Record{
			ID: id, OperatorID: o.OperatorID, ClientID: clientID, ClientRef: req.ClientRef, RequestHash: hash,
			Request: req, Decision: d, Version: 1, LocalState: d.State, Exempt: n.Exempt, Priority: n.Priority,
			TimeStart: n.TimeStart, TimeEnd: n.TimeEnd, FiledAt: at, CreatedAt: at,
			VolumesAMSL: d.VolumesAMSL, Cells: n.Cells, Envelope: envelopeOf(n), Actor: clientID, ChangeReason: "submitted",
		}
		r.Decision.Version = 1
		if err := tx.Insert(ctx, r); err != nil {
			return err
		}
		if len(flagged) > 0 {
			if err := tx.FlagUpdate(ctx, flagged, id, at); err != nil {
				return err
			}
		}
		if err := s.project(ctx, r); err != nil {
			return err
		}
		out = r.Decision
		return nil
	})
	if errors.Is(err, ErrDuplicate) {
		// A concurrent request with the same client_ref committed first.
		if d, ok, rerr := s.replay(ctx, clientID, req.ClientRef, hash); ok || rerr != nil {
			return d, false, rerr
		}
	}
	if err != nil {
		return Decision{}, false, s.txError(err)
	}
	s.count("submitted")
	return out, true, nil
}

func (s *Service) txError(err error) error {
	var se interface{ HTTPStatus() int }
	if errors.As(err, &se) {
		return err
	}
	return &UnavailableError{Dependency: "database", Detail: "the intent could not be written; nothing was changed"}
}

// replay answers a client_ref already used: the same body gets the
// decision as it stands, a different one is refused with 409.
func (s *Service) replay(ctx context.Context, clientID, ref, hash string) (Decision, bool, error) {
	prev, err := s.Store.ByClientRef(ctx, clientID, ref)
	if err != nil {
		return Decision{}, false, &UnavailableError{Dependency: "database", Detail: "the client's earlier requests could not be read"}
	}
	if prev == nil {
		return Decision{}, false, nil
	}
	if prev.RequestHash != hash {
		s.count("idempotency_conflict")
		return Decision{}, false, refuse(http.StatusConflict, "idempotency_conflict", "client_ref %q was used for a different request", ref)
	}
	s.count("replayed")
	return prev.Decision, true, nil
}

// others are the intents a decision is checked against: the active,
// non-exempt local intents near n in space and time and the peers'
// intents of the last 24 h, as the deconfliction reads them.
func (s *Service) others(ctx context.Context, tx Tx, n *Normalised, self string) ([]deconflict.Intent, error) {
	pol := s.policy().Values
	dist := pol.DeconflictBufferM
	if !core.IsFinite(dist) || dist < 0 {
		dist = 0 // the deconfliction refuses the invalid buffer itself
	}
	recs, err := tx.Overlapping(ctx, envelopeOf(n), dist+prefilterMarginM, n.TimeStart, n.TimeEnd, self, MaxOverlapping)
	if err != nil {
		return nil, err
	}
	if len(recs) > MaxOverlapping {
		s.count("overlapping_bound_exceeded")
		return nil, refuse(http.StatusServiceUnavailable, "deconfliction_bound_exceeded", "more than %d authorised intents overlap the request; it is not judged", MaxOverlapping)
	}
	out := make([]deconflict.Intent, 0, len(recs))
	for i := range recs {
		di, err := deconflictOfRecord(&recs[i])
		if err != nil {
			return nil, err
		}
		out = append(out, di)
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	peers, err := tx.PeerIntents(ctx, now.Add(-PeerMaxAge), n.TimeStart, n.TimeEnd, MaxPeerIntents)
	if err != nil {
		return nil, err
	}
	if len(peers) > MaxPeerIntents {
		s.count("peer_bound_exceeded")
		return nil, refuse(http.StatusServiceUnavailable, "deconfliction_bound_exceeded", "more than %d peer intents overlap the request; it is not judged", MaxPeerIntents)
	}
	for _, p := range peers {
		di, err := s.deconflictOfPeer(p)
		if err != nil {
			s.count("peer_intent_unjudgeable")
			return nil, refuse(http.StatusServiceUnavailable, "deconfliction_not_judged", "the peer intent %s cannot be judged: %s", p.EntityID, reasonOf(err))
		}
		out = append(out, di)
	}
	return out, nil
}

// prefilterMarginM widens the store's envelope prefilter beyond the
// buffer (the exact judgement is deconflict's).
const prefilterMarginM = 100

// envelopeOf is each volume's box padded for the store's geography
// prefilter: the box edges are parallels, a geography edge is a great
// circle, so the box grows by the bulge of its widest edge plus a margin.
func envelopeOf(n *Normalised) []geodesy.BBox {
	out := make([]geodesy.BBox, 0, len(n.Volumes))
	for i := range n.Volumes {
		out = append(out, PadForGeography(n.Volumes[i].BBox))
	}
	return out
}

// PadForGeography grows a lat/lon box so that the geography polygon of
// its four corners holds the whole box.
func PadForGeography(b geodesy.BBox) geodesy.BBox {
	const r = core.MeanEarthRadiusM
	width := b.MaxLon - b.MinLon
	if width < 0 {
		width += 360
	}
	phi := math.Max(math.Abs(b.MinLat), math.Abs(b.MaxLat))
	if phi > 89 {
		phi = 89
	}
	l := width * (math.Pi / 180) * r * math.Cos(math.Min(math.Abs(b.MinLat), math.Abs(b.MaxLat))*math.Pi/180)
	bulge := l * l * math.Tan(phi*math.Pi/180) / (8 * r)
	return b.PadM(bulge + prefilterMarginM)
}

func deconflictOfRecord(r *Record) (deconflict.Intent, error) {
	out := deconflict.Intent{ID: r.ID, Priority: r.Priority, RankAt: r.FiledAt}
	if len(r.VolumesAMSL) != len(r.Request.Volumes) {
		return out, fmt.Errorf("intent %s: %d volumes and %d AMSL bands", r.ID, len(r.Request.Volumes), len(r.VolumesAMSL))
	}
	for i, v := range r.Request.Volumes {
		dv, err := wireVolume(v, r.VolumesAMSL[i].UndulationM)
		if err != nil {
			return out, fmt.Errorf("intent %s: %w", r.ID, err)
		}
		out.Volumes = append(out.Volumes, dv)
	}
	return out, nil
}

func (s *Service) deconflictOfPeer(p PeerIntent) (deconflict.Intent, error) {
	out := deconflict.Intent{ID: "peer:" + p.EntityID, Priority: p.Priority, RankAt: p.FetchedAt}
	if s.Geoid == nil {
		return out, errors.New("no geoid")
	}
	if len(p.Volumes) == 0 || len(p.Volumes) > deconflict.MaxVolumes {
		return out, fmt.Errorf("%d volumes", len(p.Volumes))
	}
	if err := coreCheck(p.Volumes, p.FetchedAt); err != nil {
		return out, err
	}
	for _, v := range p.Volumes {
		var c core.LatLon
		switch {
		case v.Volume.OutlineCircle != nil:
			c = v.Volume.OutlineCircle.Center.LatLon()
		default:
			pts := make([]core.LatLon, len(v.Volume.OutlinePolygon.Vertices))
			for k, q := range v.Volume.OutlinePolygon.Vertices {
				pts[k] = q.LatLon()
			}
			c = centroid(pts)
		}
		und, err := s.Geoid.UndulationM(c)
		if err != nil {
			return out, err
		}
		dv, err := wireVolume(v, und)
		if err != nil {
			return out, err
		}
		out.Volumes = append(out.Volumes, dv)
	}
	return out, nil
}

// wireVolume reads a stored or peer Volume4D with the undulation of its
// outline into the deconfliction's volume.
func wireVolume(v f3548.Volume4D, undulationM float64) (deconflict.Volume, error) {
	var out deconflict.Volume
	if v.TimeStart == nil || v.TimeEnd == nil || v.Volume.AltitudeLower == nil || v.Volume.AltitudeUpper == nil {
		return out, core.Fieldf("volume", "has no window or no band")
	}
	lo, err := v.Volume.AltitudeLower.HAEM()
	if err != nil {
		return out, err
	}
	hi, err := v.Volume.AltitudeUpper.HAEM()
	if err != nil {
		return out, err
	}
	switch {
	case v.Volume.OutlineCircle != nil && v.Volume.OutlineCircle.Center != nil && v.Volume.OutlineCircle.Radius != nil:
		c := v.Volume.OutlineCircle
		out.Shape = deconflict.Shape{Circle: &geodesy.Circle{Center: c.Center.LatLon(), RadiusM: wireRadius(c.Radius.Value)}}
	case v.Volume.OutlinePolygon != nil:
		pts := make([]core.LatLon, len(v.Volume.OutlinePolygon.Vertices))
		for k, q := range v.Volume.OutlinePolygon.Vertices {
			pts[k] = q.LatLon()
		}
		out.Shape = deconflict.Shape{Polygon: pts}
	default:
		return out, core.Fieldf("volume", "has no outline")
	}
	out.LowerAMSLM, out.UpperAMSLM = geoid.AMSLFromHAE(lo, undulationM), geoid.AMSLFromHAE(hi, undulationM)
	out.Start, out.End = v.TimeStart.Value.UTC(), v.TimeEnd.Value.UTC()
	return out, nil
}

func (s *Service) project(ctx context.Context, r *Record) error {
	if s.Projector == nil {
		return &policy.ProjectionError{Bucket: "intent_active", Err: errors.New("no projector configured")}
	}
	if err := s.Projector.Project(ctx, r); err != nil {
		s.count("projection_failed")
		var pe *policy.ProjectionError
		if errors.As(err, &pe) {
			return err
		}
		return &policy.ProjectionError{Bucket: "intent_active", Err: err}
	}
	return nil
}

// View is an intent as GET answers it: the decision as it stands.
type View = Decision

// Get is GET /v1/intents/{id}: the operator's own intent only (another
// operator's is not found, never forbidden: its existence is not told).
func (s *Service) Get(ctx context.Context, clientID, id string) (Decision, error) {
	o, err := s.owner(ctx, clientID)
	if err != nil {
		return Decision{}, err
	}
	if !validUUID(id) {
		return Decision{}, refuse(http.StatusNotFound, "not_found", "no such intent")
	}
	r, err := s.Store.Get(ctx, id)
	if err != nil {
		return Decision{}, &UnavailableError{Dependency: "database", Detail: "the intent could not be read"}
	}
	if r == nil || r.OperatorID != o.OperatorID {
		return Decision{}, refuse(http.StatusNotFound, "not_found", "no such intent")
	}
	return r.Decision, nil
}

// List is GET /v1/intents: the operator's own intents, newest first, at
// most MaxList.
func (s *Service) List(ctx context.Context, clientID string, f ListFilter) ([]Decision, error) {
	o, err := s.owner(ctx, clientID)
	if err != nil {
		return nil, err
	}
	if f.State != "" && !slices.Contains(append(slices.Clone(OpenStates), StateEnded, StateRejected, StateWithdrawn), f.State) {
		return nil, core.Fieldf("state", "%q is not a state", f.State)
	}
	if f.From != nil && f.To != nil && f.To.Before(*f.From) {
		return nil, core.Fieldf("to", "is before from")
	}
	if f.Limit <= 0 || f.Limit > MaxList {
		f.Limit = MaxList
	}
	rs, err := s.Store.List(ctx, o.OperatorID, f)
	if err != nil {
		return nil, &UnavailableError{Dependency: "database", Detail: "the intents could not be read"}
	}
	out := make([]Decision, len(rs))
	for i := range rs {
		out[i] = rs[i].Decision
	}
	return out, nil
}

// Patch is the body of PATCH /v1/intents/{id}.
type Patch struct {
	Action       string           `json:"action"`
	Volumes      []f3548.Volume4D `json:"volumes,omitempty"`
	ChangeReason string           `json:"change_reason,omitempty"`
}

// Change is PATCH /v1/intents/{id}: activate (confirmed in the
// response, Art. 10(5)), modify (new volumes re-decided as a new
// version, keeping the authorisation number when still authorised, Art.
// 6(6)) or end.
func (s *Service) Change(ctx context.Context, clientID, id string, raw []byte) (Decision, error) {
	var p Patch
	if len(raw) > MaxRequestBytes {
		return Decision{}, core.Fieldf("body", "longer than %d bytes", MaxRequestBytes)
	}
	if err := strictDecode(raw, &p); err != nil {
		return Decision{}, err
	}
	if p.ChangeReason != "" {
		ps := &problems{}
		ps.text("change_reason", p.ChangeReason, 256)
		if pr := ps.result(); pr != nil {
			return Decision{}, &Error{Status: http.StatusBadRequest, Slug: "validation", Detail: "change_reason is not usable", Problems: pr}
		}
	}
	switch p.Action {
	case ActionActivate, ActionEnd:
		if len(p.Volumes) > 0 {
			return Decision{}, core.Fieldf("volumes", "only modify takes volumes")
		}
	case ActionModify:
		if len(p.Volumes) == 0 {
			return Decision{}, core.Fieldf("volumes", "required to modify")
		}
	default:
		return Decision{}, core.Fieldf("action", "must be activate, modify or end")
	}
	o, err := s.owner(ctx, clientID)
	if err != nil {
		return Decision{}, err
	}
	if !validUUID(id) {
		return Decision{}, refuse(http.StatusNotFound, "not_found", "no such intent")
	}
	cur, err := s.Store.Get(ctx, id)
	if err != nil {
		return Decision{}, &UnavailableError{Dependency: "database", Detail: "the intent could not be read"}
	}
	if cur == nil || cur.OperatorID != o.OperatorID {
		return Decision{}, refuse(http.StatusNotFound, "not_found", "no such intent")
	}
	if p.Action == ActionModify {
		return s.modify(ctx, o, cur, p)
	}
	var out Decision
	err = s.Store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		r, err := tx.Lock(ctx, id)
		if err != nil {
			return err
		}
		if r == nil {
			return refuse(http.StatusNotFound, "not_found", "no such intent")
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		event := EventActivated
		if p.Action == ActionActivate {
			if err := s.activatable(r, now); err != nil {
				return err
			}
			r.LocalState = StateActivated
			if r.Decision.DSSState != nil {
				r.Decision.DSSState = ptr("Activated")
			}
			r.ChangeReason = "activated by the operator"
		} else {
			if !slices.Contains(OpenStates, r.LocalState) {
				s.count("end_refused")
				return refuse(http.StatusConflict, "end_refused", "the intent is %s and cannot be ended", r.LocalState)
			}
			event = EventEnded
			r.LocalState = StateEnded
			r.Decision.DSSState = nil
			r.ChangeReason = "ended by the operator"
		}
		if p.ChangeReason != "" {
			r.ChangeReason = p.ChangeReason
		}
		r.Actor = clientID
		s.advance(r, now)
		if err := tx.Update(ctx, r, event); err != nil {
			return err
		}
		if err := s.project(ctx, r); err != nil {
			return err
		}
		out = r.Decision
		return nil
	})
	if err != nil {
		return Decision{}, s.txError(err)
	}
	if p.Action == ActionActivate {
		s.count("activated")
	} else {
		s.count("ended")
	}
	return out, nil
}

// activatable refuses an activation outside the state or the window:
// only an accepted intent, from time_start - activation_lead_s to
// time_end, on the database clock.
func (s *Service) activatable(r *Record, now time.Time) error {
	if r.LocalState != StateAccepted {
		s.count("activation_refused")
		return refuse(http.StatusConflict, "activation_refused", "the intent is %s; only an accepted intent is activated", r.LocalState)
	}
	lead := time.Duration(s.policy().Values.ActivationLeadS * float64(time.Second))
	if now.Before(r.TimeStart.Add(-lead)) {
		s.count("activation_refused")
		return refuse(http.StatusConflict, "activation_refused", "activation opens at %s, %s before time_start", r.TimeStart.Add(-lead).UTC().Format(time.RFC3339), lead)
	}
	if now.After(r.TimeEnd) {
		s.count("activation_refused")
		return refuse(http.StatusConflict, "activation_refused", "time_end %s has passed", r.TimeEnd.UTC().Format(time.RFC3339))
	}
	return nil
}

// advance moves r to its next version with the state set, mirrored in
// its decision body.
func (s *Service) advance(r *Record, now time.Time) {
	r.Version++
	r.Decision.Version = r.Version
	r.Decision.State = r.LocalState
	r.Decision.UpdatedAt = now
	if r.ChangeReason != "" {
		r.Decision.ChangeReason = ptr(r.ChangeReason)
	}
}

func (s *Service) modify(ctx context.Context, o Owner, cur *Record, p Patch) (Decision, error) {
	if cur.LocalState != StateAccepted {
		s.count("modify_refused")
		return Decision{}, refuse(http.StatusConflict, "modify_refused", "the intent is %s; only an accepted intent is modified (end it and file a new one)", cur.LocalState)
	}
	req := cur.Request
	req.Volumes = p.Volumes
	pol := s.policy()
	now, err := s.Store.Now(ctx)
	if err != nil {
		return Decision{}, &UnavailableError{Dependency: "database", Detail: "the database clock could not be read"}
	}
	n, err := s.validate(ctx, o, req, pol, now)
	if err != nil {
		return Decision{}, err
	}
	a := s.Decider.Assess(ctx, n, pol, now)
	if cur.Decision.AuthorisationNumber != nil {
		a.KeepNumber(*cur.Decision.AuthorisationNumber)
	}
	var out Decision
	err = s.Store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		r, err := tx.Lock(ctx, cur.ID)
		if err != nil {
			return err
		}
		if r == nil || r.Version != cur.Version || r.LocalState != StateAccepted {
			return refuse(http.StatusConflict, "modify_refused", "the intent changed while the modification was judged; read it and try again")
		}
		at, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		others, err := s.others(ctx, tx, n, r.ID)
		if err != nil {
			return err
		}
		d, flagged := s.Decider.Finish(a, r.ID, at, others)
		if d.Decision != DecisionAuthorised && d.Decision != DecisionAcceptedVoluntary {
			d.AuthorisationNumber = nil
		}
		reason := "modified by the operator"
		if p.ChangeReason != "" {
			reason = p.ChangeReason
		}
		d.ClientRef = r.ClientRef
		d.DecidedAt = r.Decision.DecidedAt
		r.Request, r.Decision = req, d
		r.LocalState = d.State
		r.TimeStart, r.TimeEnd, r.FiledAt = n.TimeStart, n.TimeEnd, at
		r.VolumesAMSL, r.Cells, r.Envelope = d.VolumesAMSL, n.Cells, envelopeOf(n)
		r.Priority, r.Exempt = n.Priority, n.Exempt
		r.ChangeReason, r.Actor = reason, o.ClientID
		s.advance(r, at)
		if err := tx.Update(ctx, r, EventModified); err != nil {
			return err
		}
		if len(flagged) > 0 {
			if err := tx.FlagUpdate(ctx, flagged, r.ID, at); err != nil {
				return err
			}
		}
		if err := s.project(ctx, r); err != nil {
			return err
		}
		out = r.Decision
		return nil
	})
	if err != nil {
		return Decision{}, s.txError(err)
	}
	s.count("modified")
	return out, nil
}

// EndDue ends the open intents whose time_end has passed (on the
// database clock), at most SweepBatch, and returns how many.
func (s *Service) EndDue(ctx context.Context) (int, error) {
	n := 0
	err := s.Store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		due, err := tx.DueToEnd(ctx, now, SweepBatch)
		if err != nil {
			return err
		}
		for i := range due {
			r := &due[i]
			r.LocalState, r.Decision.DSSState = StateEnded, nil
			r.ChangeReason, r.Actor = "time_end passed", "system"
			s.advance(r, now)
			if err := tx.Update(ctx, r, EventExpired); err != nil {
				return err
			}
			if err := s.project(ctx, r); err != nil {
				return err
			}
		}
		n = len(due)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if s.Counters != nil {
		s.Counters.Add("intent_expired", uint64(n))
	}
	return n, nil
}

// RunSweep ends due intents every interval until ctx ends.
func (s *Service) RunSweep(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := s.EndDue(ctx); err != nil {
			obs.Error(ctx, s.logger(), "intents past time_end not ended", err)
		} else if n > 0 {
			s.logger().Info("intents past time_end ended", slog.Int("ended", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !lowerOrDigit(c) || c > 'f' {
				return false
			}
		}
	}
	return true
}
