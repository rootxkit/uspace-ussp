package fakedss

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"
)

// The F3548 side of the fake (WP-13), under /dss/v1, with the DSS's
// semantics as the pinned InterUSS DSS implements them
// (pkg/scd/operational_intents_handler.go at the commit of
// uspace-lab deploy/dss/SOURCE): an operational intent reference is
// created only Accepted, updated or deleted only at its current ovn and by
// its manager; a write in Accepted or Activated must carry in its key the
// ovn of every other reference its extents meet that still needs one (not
// an Accepted one of a USS whose availability is Down), and of every
// constraint they meet when its subscription tells of constraints, or it
// is answered 409 with an AirspaceConflictResponse naming the ones
// missing (their ovns scrubbed unless the caller manages them); every
// write answers the subscribers whose subscriptions meet it, each index
// advanced. The manager of an entity is the sub of the caller's token.

// NoOVN is what the DSS writes in place of an ovn it will not tell
// (scdmodels.NoOvnPhrase).
const NoOVN = "Available from USS"

type oir struct {
	ref   f3548.OperationalIntentReference
	ext   []f3548.Volume4D
	boxes []geodesy.BBox
	start time.Time
	end   time.Time
}

type cstr struct {
	ref   f3548.ConstraintReference
	boxes []geodesy.BBox
	start time.Time
	end   time.Time
}

type utmSub struct {
	s        f3548.Subscription
	owner    string
	box      geodesy.BBox
	start    time.Time
	end      time.Time
	implicit bool
}

type utm struct {
	oirs  map[string]*oir
	cstrs map[string]*cstr
	subs  map[string]*utmSub
	avail map[string]f3548.UssAvailabilityState
}

func newUTM() *utm {
	return &utm{oirs: map[string]*oir{}, cstrs: map[string]*cstr{}, subs: map[string]*utmSub{}, avail: map[string]f3548.UssAvailabilityState{}}
}

// subOf reads the sub of a bearer JWT without verifying it (the fake
// trusts its callers; the real DSS verifies the signature); Owner when the
// token is not a JWT.
func (d *DSS) subOf(authz string) string {
	tok := strings.TrimPrefix(authz, "Bearer ")
	parts := strings.Split(tok, ".")
	if len(parts) == 3 {
		if b, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var c struct {
				Sub string `json:"sub"`
			}
			if json.Unmarshal(b, &c) == nil && c.Sub != "" {
				return c.Sub
			}
		}
	}
	return d.Owner
}

func failUTM(w http.ResponseWriter, status int, m string) {
	write(w, status, f3548.ErrorResponse{Message: &m})
}

func newOVN() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func t3548(t time.Time) f3548.Time { return f3548.Time{Format: f3548.RFC3339, Value: t.UTC()} }

// window4 bounds F3548 extents: every volume needs its window and band,
// and the end may not be in the past.
func (d *DSS) window4(ext []f3548.Volume4D, needBand bool) ([]geodesy.BBox, time.Time, time.Time, string) {
	if len(ext) == 0 {
		return nil, time.Time{}, time.Time{}, "no extents"
	}
	var boxes []geodesy.BBox
	var from, to time.Time
	for i, v := range ext {
		box, s, e, err := f3548.Volume4DToZonesEnvelope(v)
		if err != nil {
			return nil, from, to, err.Error()
		}
		if s.IsZero() || e.IsZero() {
			return nil, from, to, "a volume without time_start or time_end"
		}
		if needBand && (v.Volume.AltitudeLower == nil || v.Volume.AltitudeUpper == nil) {
			return nil, from, to, "a volume without its altitudes"
		}
		boxes = append(boxes, box)
		if i == 0 || s.Before(from) {
			from = s
		}
		if i == 0 || e.After(to) {
			to = e
		}
	}
	if !to.After(d.now()) {
		return nil, from, to, "the extents end in the past"
	}
	return boxes, from, to, ""
}

func meets(a []geodesy.BBox, b []geodesy.BBox) bool {
	for _, x := range a {
		for _, y := range b {
			if overlaps(x, y) {
				return true
			}
		}
	}
	return false
}

func within(as, ae, bs, be time.Time) bool { return !ae.Before(bs) && !be.Before(as) }

func (d *DSS) serve3548(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	who := d.subOf(r.Header.Get("Authorization"))
	n := len(parts)
	switch {
	case parts[0] == "operational_intent_references" && n == 2 && parts[1] == "query" && r.Method == http.MethodPost:
		d.queryOIRs(w, r, who)
	case parts[0] == "operational_intent_references" && n == 2 && r.Method == http.MethodGet:
		d.getOIR(w, parts[1], who)
	case parts[0] == "operational_intent_references" && n == 2 && r.Method == http.MethodPut:
		d.putOIR(w, r, who, parts[1], "")
	case parts[0] == "operational_intent_references" && n == 3 && r.Method == http.MethodPut:
		d.putOIR(w, r, who, parts[1], parts[2])
	case parts[0] == "operational_intent_references" && n == 3 && r.Method == http.MethodDelete:
		d.deleteOIR(w, who, parts[1], parts[2])
	case parts[0] == "constraint_references" && n == 2 && parts[1] == "query" && r.Method == http.MethodPost:
		d.queryConstraints(w, r, who)
	case parts[0] == "constraint_references" && n == 2 && r.Method == http.MethodPut:
		d.putConstraint(w, r, who, parts[1], "")
	case parts[0] == "constraint_references" && n == 3 && r.Method == http.MethodPut:
		d.putConstraint(w, r, who, parts[1], parts[2])
	case parts[0] == "constraint_references" && n == 3 && r.Method == http.MethodDelete:
		d.deleteConstraint(w, who, parts[1], parts[2])
	case parts[0] == "subscriptions" && n == 2 && r.Method == http.MethodGet:
		d.getUTMSub(w, who, parts[1])
	case parts[0] == "subscriptions" && n == 2 && r.Method == http.MethodPut:
		d.putUTMSub(w, r, who, parts[1], "")
	case parts[0] == "subscriptions" && n == 3 && r.Method == http.MethodPut:
		d.putUTMSub(w, r, who, parts[1], parts[2])
	case parts[0] == "subscriptions" && n == 3 && r.Method == http.MethodDelete:
		d.deleteUTMSub(w, who, parts[1], parts[2])
	case parts[0] == "uss_availability" && n == 2 && r.Method == http.MethodGet:
		d.getAvailability(w, parts[1])
	default:
		failUTM(w, http.StatusNotFound, "not served by the fake DSS")
	}
}

// view is ref as caller sees it: the ovn only for its manager.
func view(ref f3548.OperationalIntentReference, caller string) f3548.OperationalIntentReference {
	if ref.Manager != caller {
		s := NoOVN
		ref.Ovn = &s
	}
	return ref
}

func viewC(ref f3548.ConstraintReference, caller string) f3548.ConstraintReference {
	if ref.Manager != caller {
		s := NoOVN
		ref.Ovn = &s
	}
	return ref
}

func (d *DSS) aoi(r *http.Request) ([]geodesy.BBox, time.Time, time.Time, bool) {
	var p f3548.QueryOperationalIntentReferenceParameters
	if err := decode(r, &p); err != nil || p.AreaOfInterest == nil {
		return nil, time.Time{}, time.Time{}, false
	}
	box, s, e, err := f3548.Volume4DToZonesEnvelope(*p.AreaOfInterest)
	if err != nil {
		return nil, s, e, false
	}
	if s.IsZero() {
		s = d.now().Add(-24 * time.Hour)
	}
	if e.IsZero() {
		e = d.now().Add(30 * 24 * time.Hour)
	}
	return []geodesy.BBox{box}, s, e, true
}

func (d *DSS) queryOIRs(w http.ResponseWriter, r *http.Request, who string) {
	boxes, s, e, ok := d.aoi(r)
	if !ok {
		failUTM(w, http.StatusBadRequest, "not a QueryOperationalIntentReferenceParameters with an area_of_interest")
		return
	}
	out := []f3548.OperationalIntentReference{}
	for _, o := range d.sortedOIRs() {
		if meets(boxes, o.boxes) && within(s, e, o.start, o.end) {
			out = append(out, view(o.ref, who))
		}
	}
	write(w, http.StatusOK, f3548.QueryOperationalIntentReferenceResponse{OperationalIntentReferences: out})
}

func (d *DSS) sortedOIRs() []*oir {
	out := make([]*oir, 0, len(d.utm.oirs))
	for _, o := range d.utm.oirs {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ref.Id < out[j].ref.Id })
	return out
}

func (d *DSS) getOIR(w http.ResponseWriter, id, who string) {
	o, ok := d.utm.oirs[id]
	if !ok {
		failUTM(w, http.StatusNotFound, "operational intent reference "+id+" not found")
		return
	}
	write(w, http.StatusOK, f3548.GetOperationalIntentReferenceResponse{OperationalIntentReference: view(o.ref, who)})
}

func requiresKey(s f3548.OperationalIntentState) bool {
	return s != f3548.Nonconforming && s != f3548.Contingent
}

func (d *DSS) oirRequiresKey(o *oir) bool {
	return d.utm.avail[o.ref.Manager] != f3548.Down || o.ref.State != f3548.Accepted
}

func (d *DSS) putOIR(w http.ResponseWriter, r *http.Request, who, id, ovn string) {
	var p f3548.PutOperationalIntentReferenceParameters
	if err := decode(r, &p); err != nil || p.UssBaseUrl == "" {
		failUTM(w, http.StatusBadRequest, "not PutOperationalIntentReferenceParameters with a uss_base_url")
		return
	}
	if !slices.Contains(f3548.DSSStates, p.State) {
		failUTM(w, http.StatusBadRequest, "invalid state "+string(p.State))
		return
	}
	if p.SubscriptionId != nil && p.NewSubscription != nil {
		failUTM(w, http.StatusBadRequest, "cannot provide both a subscription id and an implicit subscription")
		return
	}
	boxes, start, end, bad := d.window4(p.Extents, true)
	if bad != "" {
		failUTM(w, http.StatusBadRequest, "invalid extents: "+bad)
		return
	}
	old, exists := d.utm.oirs[id]
	switch {
	case ovn == "" && p.State != f3548.Accepted:
		failUTM(w, http.StatusBadRequest, "invalid state for initial version: "+string(p.State))
		return
	case exists && old.ref.Manager != who:
		failUTM(w, http.StatusForbidden, "the operational intent is managed by another USS")
		return
	case exists && (old.ref.Ovn == nil || *old.ref.Ovn != ovn):
		write(w, http.StatusConflict, f3548.AirspaceConflictResponse{Message: ptr("current version is not " + ovn)})
		return
	case !exists && ovn != "":
		failUTM(w, http.StatusNotFound, "operational intent "+id+" does not exist and therefore is not version "+ovn)
		return
	case !exists && len(d.utm.oirs) >= MaxEntries:
		failUTM(w, http.StatusTooManyRequests, "the fake DSS is full")
		return
	}
	// The subscription: the one named (ours), or an implicit one.
	var sub *utmSub
	switch {
	case p.SubscriptionId != nil:
		s, ok := d.utm.subs[*p.SubscriptionId]
		if !ok {
			failUTM(w, http.StatusBadRequest, "subscription "+*p.SubscriptionId+" does not exist")
			return
		}
		if s.owner != who {
			failUTM(w, http.StatusForbidden, "the subscription is owned by another USS")
			return
		}
		sub = s
	case p.NewSubscription != nil:
		if p.NewSubscription.UssBaseUrl == "" {
			failUTM(w, http.StatusBadRequest, "an implicit subscription without uss_base_url")
			return
		}
		yes, forC := true, p.NewSubscription.NotifyForConstraints != nil && *p.NewSubscription.NotifyForConstraints
		zero := int32(0)
		ts, te := t3548(start), t3548(end)
		sub = &utmSub{s: f3548.Subscription{Id: newUUID(), UssBaseUrl: p.NewSubscription.UssBaseUrl, Version: newOVN(), NotificationIndex: zero,
			NotifyForOperationalIntents: &yes, NotifyForConstraints: &forC, ImplicitSubscription: &yes, TimeStart: &ts, TimeEnd: &te},
			box: union(boxes), start: start, end: end, implicit: true, owner: who}
	case exists && old.ref.SubscriptionId != "":
		sub = d.utm.subs[old.ref.SubscriptionId]
	}
	if sub == nil && p.State != f3548.Accepted {
		failUTM(w, http.StatusBadRequest, "state "+string(p.State)+" requires a subscription")
		return
	}
	if requiresKey(p.State) {
		key := map[string]bool{}
		if p.Key != nil {
			for _, k := range *p.Key {
				key[k] = true
			}
		}
		var missingO []f3548.OperationalIntentReference
		for _, o := range d.sortedOIRs() {
			if o.ref.Id == id || !meets(boxes, o.boxes) || !within(start, end, o.start, o.end) || !d.oirRequiresKey(o) {
				continue
			}
			if o.ref.Ovn == nil || !key[*o.ref.Ovn] {
				missingO = append(missingO, view(o.ref, who))
			}
		}
		var missingC []f3548.ConstraintReference
		if sub != nil && sub.s.NotifyForConstraints != nil && *sub.s.NotifyForConstraints {
			for _, c := range d.sortedCstrs() {
				if !meets(boxes, c.boxes) || !within(start, end, c.start, c.end) {
					continue
				}
				if c.ref.Ovn == nil || !key[*c.ref.Ovn] {
					missingC = append(missingC, viewC(c.ref, who))
				}
			}
		}
		if len(missingO) > 0 || len(missingC) > 0 {
			ans := f3548.AirspaceConflictResponse{Message: ptr("Current OVNs not provided for one or more OperationalIntents or Constraints")}
			if len(missingO) > 0 {
				ans.MissingOperationalIntents = &missingO
			}
			if len(missingC) > 0 {
				ans.MissingConstraints = &missingC
			}
			d.conflicts++
			write(w, http.StatusConflict, ans)
			return
		}
	}
	if sub != nil {
		if sub.implicit {
			sub.box = union(append(boxes, sub.box))
			if start.Before(sub.start) {
				sub.start = start
			}
			if end.After(sub.end) {
				sub.end = end
			}
		}
		d.utm.subs[sub.s.Id] = sub
	}
	version := int32(1)
	if exists {
		version = old.ref.Version + 1
	}
	o := &oir{ext: p.Extents, boxes: boxes, start: start, end: end}
	newOvn := newOVN()
	o.ref = f3548.OperationalIntentReference{Id: id, Manager: who, Ovn: &newOvn, State: p.State, TimeStart: t3548(start), TimeEnd: t3548(end),
		UssAvailability: d.availOf(who), UssBaseUrl: p.UssBaseUrl, Version: version}
	if sub != nil {
		o.ref.SubscriptionId = sub.s.Id
	}
	d.utm.oirs[id] = o
	subs := d.utmSubscribers(append(boxes, boxesOf(old)...), start, end, true)
	status := http.StatusOK
	if !exists {
		status = http.StatusCreated
	}
	write(w, status, f3548.ChangeOperationalIntentReferenceResponse{OperationalIntentReference: o.ref, Subscribers: subs})
}

func boxesOf(o *oir) []geodesy.BBox {
	if o == nil {
		return nil
	}
	return o.boxes
}

func ptr[T any](v T) *T { return &v }

func union(bs []geodesy.BBox) geodesy.BBox {
	out := bs[0]
	for _, b := range bs[1:] {
		out.MinLat, out.MinLon = min(out.MinLat, b.MinLat), min(out.MinLon, b.MinLon)
		out.MaxLat, out.MaxLon = max(out.MaxLat, b.MaxLat), max(out.MaxLon, b.MaxLon)
	}
	return out
}

func (d *DSS) availOf(uss string) f3548.UssAvailabilityState {
	if a, ok := d.utm.avail[uss]; ok {
		return a
	}
	return f3548.Unknown
}

// utmSubscribers are the subscriptions that meet boxes in [start, end]
// and tell of operational intents (oi) or of constraints, each index
// advanced, grouped by base URL.
func (d *DSS) utmSubscribers(boxes []geodesy.BBox, start, end time.Time, oi bool) []f3548.SubscriberToNotify {
	byURL := map[string]*f3548.SubscriberToNotify{}
	var order []string
	ids := make([]string, 0, len(d.utm.subs))
	for id := range d.utm.subs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := d.utm.subs[id]
		if !meets(boxes, []geodesy.BBox{s.box}) || end.Before(s.start) || start.After(s.end) {
			continue
		}
		tell := s.s.NotifyForOperationalIntents
		if !oi {
			tell = s.s.NotifyForConstraints
		}
		if tell == nil || !*tell {
			continue
		}
		s.s.NotificationIndex++
		u := s.s.UssBaseUrl
		if byURL[u] == nil {
			byURL[u] = &f3548.SubscriberToNotify{UssBaseUrl: u}
			order = append(order, u)
		}
		byURL[u].Subscriptions = append(byURL[u].Subscriptions, f3548.SubscriptionState{SubscriptionId: s.s.Id, NotificationIndex: s.s.NotificationIndex})
	}
	out := make([]f3548.SubscriberToNotify, 0, len(order))
	for _, u := range order {
		out = append(out, *byURL[u])
	}
	return out
}

func (d *DSS) deleteOIR(w http.ResponseWriter, who, id, ovn string) {
	o, ok := d.utm.oirs[id]
	switch {
	case !ok:
		failUTM(w, http.StatusNotFound, "operational intent reference "+id+" not found")
		return
	case o.ref.Manager != who:
		failUTM(w, http.StatusForbidden, "the operational intent is managed by another USS")
		return
	case o.ref.Ovn == nil || *o.ref.Ovn != ovn:
		write(w, http.StatusConflict, f3548.AirspaceConflictResponse{Message: ptr("current version is not " + ovn)})
		return
	}
	delete(d.utm.oirs, id)
	if s, ok := d.utm.subs[o.ref.SubscriptionId]; ok && s.implicit {
		dependent := false
		for _, x := range d.utm.oirs {
			if x.ref.SubscriptionId == s.s.Id {
				dependent = true
			}
		}
		if !dependent {
			delete(d.utm.subs, s.s.Id)
		}
	}
	subs := d.utmSubscribers(o.boxes, o.start, o.end, true)
	write(w, http.StatusOK, f3548.ChangeOperationalIntentReferenceResponse{OperationalIntentReference: o.ref, Subscribers: subs})
}

func (d *DSS) sortedCstrs() []*cstr {
	out := make([]*cstr, 0, len(d.utm.cstrs))
	for _, c := range d.utm.cstrs {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ref.Id < out[j].ref.Id })
	return out
}

func (d *DSS) queryConstraints(w http.ResponseWriter, r *http.Request, who string) {
	boxes, s, e, ok := d.aoi(r)
	if !ok {
		failUTM(w, http.StatusBadRequest, "not a QueryConstraintReferenceParameters with an area_of_interest")
		return
	}
	out := []f3548.ConstraintReference{}
	for _, c := range d.sortedCstrs() {
		if meets(boxes, c.boxes) && within(s, e, c.start, c.end) {
			out = append(out, viewC(c.ref, who))
		}
	}
	write(w, http.StatusOK, f3548.QueryConstraintReferencesResponse{ConstraintReferences: out})
}

func (d *DSS) putConstraint(w http.ResponseWriter, r *http.Request, who, id, ovn string) {
	var p f3548.PutConstraintReferenceParameters
	if err := decode(r, &p); err != nil || p.UssBaseUrl == "" {
		failUTM(w, http.StatusBadRequest, "not PutConstraintReferenceParameters with a uss_base_url")
		return
	}
	boxes, start, end, bad := d.window4(p.Extents, false)
	if bad != "" {
		failUTM(w, http.StatusBadRequest, "invalid extents: "+bad)
		return
	}
	old, exists := d.utm.cstrs[id]
	switch {
	case exists && old.ref.Manager != who:
		failUTM(w, http.StatusForbidden, "the constraint is managed by another USS")
		return
	case exists && (old.ref.Ovn == nil || *old.ref.Ovn != ovn):
		failUTM(w, http.StatusConflict, "current version is not "+ovn)
		return
	case !exists && ovn != "":
		failUTM(w, http.StatusNotFound, "constraint "+id+" not found")
		return
	}
	version := int32(1)
	if exists {
		version = old.ref.Version + 1
	}
	o := newOVN()
	c := &cstr{boxes: boxes, start: start, end: end, ref: f3548.ConstraintReference{Id: id, Manager: who, Ovn: &o, TimeStart: t3548(start),
		TimeEnd: t3548(end), UssAvailability: d.availOf(who), UssBaseUrl: p.UssBaseUrl, Version: version}}
	d.utm.cstrs[id] = c
	subs := d.utmSubscribers(boxes, start, end, false)
	status := http.StatusOK
	if !exists {
		status = http.StatusCreated
	}
	write(w, status, f3548.ChangeConstraintReferenceResponse{ConstraintReference: c.ref, Subscribers: subs})
}

func (d *DSS) deleteConstraint(w http.ResponseWriter, who, id, ovn string) {
	c, ok := d.utm.cstrs[id]
	switch {
	case !ok:
		failUTM(w, http.StatusNotFound, "constraint "+id+" not found")
		return
	case c.ref.Manager != who:
		failUTM(w, http.StatusForbidden, "the constraint is managed by another USS")
		return
	case c.ref.Ovn == nil || *c.ref.Ovn != ovn:
		failUTM(w, http.StatusConflict, "current version is not "+ovn)
		return
	}
	delete(d.utm.cstrs, id)
	write(w, http.StatusOK, f3548.ChangeConstraintReferenceResponse{ConstraintReference: c.ref, Subscribers: d.utmSubscribers(c.boxes, c.start, c.end, false)})
}

func (d *DSS) getUTMSub(w http.ResponseWriter, who, id string) {
	s, ok := d.utm.subs[id]
	if !ok || s.owner != who {
		failUTM(w, http.StatusNotFound, "subscription "+id+" not found")
		return
	}
	write(w, http.StatusOK, f3548.GetSubscriptionResponse{Subscription: s.s})
}

func (d *DSS) putUTMSub(w http.ResponseWriter, r *http.Request, who, id, version string) {
	var p f3548.PutSubscriptionParameters
	if err := decode(r, &p); err != nil || p.UssBaseUrl == "" {
		failUTM(w, http.StatusBadRequest, "not PutSubscriptionParameters with a uss_base_url")
		return
	}
	boxes, start, end, bad := d.window4([]f3548.Volume4D{p.Extents}, false)
	if bad != "" {
		failUTM(w, http.StatusBadRequest, "invalid extents: "+bad)
		return
	}
	if end.Sub(start) > time.Duration(f3548.DSSMaxSubscriptionDurationHours)*time.Hour {
		failUTM(w, http.StatusBadRequest, "a subscription longer than 24 h")
		return
	}
	old, exists := d.utm.subs[id]
	switch {
	case exists && old.owner != who:
		failUTM(w, http.StatusForbidden, "the subscription is owned by another USS")
		return
	case exists && version == "":
		failUTM(w, http.StatusConflict, "subscription "+id+" already exists")
		return
	case exists && old.s.Version != version:
		failUTM(w, http.StatusConflict, "version "+version+" is not the current one")
		return
	case !exists && version != "":
		failUTM(w, http.StatusNotFound, "subscription "+id+" not found")
		return
	case !exists && len(d.utm.subs) >= MaxEntries:
		failUTM(w, http.StatusTooManyRequests, "the fake DSS is full")
		return
	}
	idx := int32(0)
	if exists {
		idx = old.s.NotificationIndex
	}
	ts, te := t3548(start), t3548(end)
	no := false
	s := &utmSub{s: f3548.Subscription{Id: id, UssBaseUrl: p.UssBaseUrl, Version: newOVN(), NotificationIndex: idx,
		NotifyForOperationalIntents: p.NotifyForOperationalIntents, NotifyForConstraints: p.NotifyForConstraints,
		ImplicitSubscription: &no, TimeStart: &ts, TimeEnd: &te}, box: union(boxes), start: start, end: end, owner: who}
	d.utm.subs[id] = s
	var ois []f3548.OperationalIntentReference
	for _, o := range d.sortedOIRs() {
		if meets(boxes, o.boxes) && within(start, end, o.start, o.end) {
			ois = append(ois, view(o.ref, who))
		}
	}
	var cs []f3548.ConstraintReference
	for _, c := range d.sortedCstrs() {
		if meets(boxes, c.boxes) && within(start, end, c.start, c.end) {
			cs = append(cs, viewC(c.ref, who))
		}
	}
	write(w, http.StatusOK, f3548.PutSubscriptionResponse{Subscription: s.s, OperationalIntentReferences: &ois, ConstraintReferences: &cs})
}

func (d *DSS) deleteUTMSub(w http.ResponseWriter, who, id, version string) {
	s, ok := d.utm.subs[id]
	switch {
	case !ok:
		failUTM(w, http.StatusNotFound, "subscription "+id+" not found")
		return
	case s.owner != who:
		failUTM(w, http.StatusForbidden, "the subscription is owned by another USS")
		return
	case s.s.Version != version:
		failUTM(w, http.StatusConflict, "version "+version+" is not the current one")
		return
	}
	delete(d.utm.subs, id)
	write(w, http.StatusOK, f3548.DeleteSubscriptionResponse{Subscription: s.s})
}

func (d *DSS) getAvailability(w http.ResponseWriter, uss string) {
	write(w, http.StatusOK, f3548.UssAvailabilityStatusResponse{Status: f3548.UssAvailabilityStatus{Uss: uss, Availability: d.availOf(uss)}, Version: "1"})
}

// SetAvailability sets a USS's availability as the authority would
// (PUT /dss/v1/uss_availability/{uss_id}).
func (d *DSS) SetAvailability(uss string, a f3548.UssAvailabilityState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.utm.avail[uss] = a
	for _, o := range d.utm.oirs {
		if o.ref.Manager == uss {
			o.ref.UssAvailability = a
		}
	}
}

// OIR is the operational intent reference the fake holds and its
// extents; false when it holds none.
func (d *DSS) OIR(id string) (f3548.OperationalIntentReference, []f3548.Volume4D, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	o, ok := d.utm.oirs[id]
	if !ok {
		return f3548.OperationalIntentReference{}, nil, false
	}
	return o.ref, slices.Clone(o.ext), true
}

// OIRs are the ids of the operational intent references held.
func (d *DSS) OIRs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.utm.oirs))
	for id := range d.utm.oirs {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// UTMSubscriptions are the F3548 subscriptions held, by id.
func (d *DSS) UTMSubscriptions() map[string]f3548.Subscription {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]f3548.Subscription, len(d.utm.subs))
	for id, s := range d.utm.subs {
		out[id] = s.s
	}
	return out
}

// Conflicts is how many writes the fake answered 409 for a key that
// lacked an ovn.
func (d *DSS) Conflicts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conflicts
}
