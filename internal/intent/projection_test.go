package intent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

type fakeKV struct {
	err  error
	puts map[string][]byte
	dels []string
}

func (k *fakeKV) PutJSON(_ context.Context, bucket, key string, v any) error {
	if k.err != nil {
		return k.err
	}
	b, _ := json.Marshal(v)
	if k.puts == nil {
		k.puts = map[string][]byte{}
	}
	k.puts[bucket+"/"+key] = b
	return nil
}

func (k *fakeKV) Delete(_ context.Context, bucket, key string) error {
	if k.err != nil {
		return k.err
	}
	k.dels = append(k.dels, bucket+"/"+key)
	return nil
}

type fakePub struct {
	err      error
	subjects []string
	msgs     []bus.Enveloped
}

func (p *fakePub) Publish(_ context.Context, subject string, m bus.Enveloped) error {
	if p.err != nil {
		return p.err
	}
	if err := m.Head().Validate(); err != nil {
		return err
	}
	p.subjects = append(p.subjects, subject)
	p.msgs = append(p.msgs, m)
	return nil
}

// An active intent is put in intent_active and published with the
// envelope; an ended one is deleted and published; either side failing
// is a ProjectionError, which after the commit leaves the intent stored
// and marked unprojected for Republish.
func TestBusProjector(t *testing.T) {
	s, st, _ := newService(newRig())
	kv, pub := &fakeKV{}, &fakePub{}
	s.Projector = BusProjector{KV: kv, Pub: pub, Now: func() time.Time { return testNow }}
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := kv.puts[bus.BucketIntentActive+"/"+d.IntentID]
	if !ok || len(pub.subjects) != 1 || pub.subjects[0] != "intent.v1.accepted."+d.IntentID {
		t.Fatalf("puts %v subjects %v", kv.puts, pub.subjects)
	}
	var body StateBody
	if err := strictDecode(raw, &body); err != nil || body.LocalState != StateAccepted || body.IntentID != d.IntentID {
		t.Fatalf("%v %+v", err, body)
	}
	m := pub.msgs[0].(*StateMessage)
	if m.Schema != SchemaState || m.Producer != Producer {
		t.Fatalf("%+v", m.Envelope)
	}
	st.now = t0.Add(-time.Minute)
	if _, err := patch(t, s, d.IntentID, map[string]any{"action": "end"}); err != nil {
		t.Fatal(err)
	}
	if len(kv.dels) != 1 || pub.subjects[1] != "intent.v1.ended."+d.IntentID {
		t.Fatalf("dels %v subjects %v", kv.dels, pub.subjects)
	}
	for name, p := range map[string]BusProjector{
		"kv":  {KV: &fakeKV{err: errors.New("timeout")}, Pub: &fakePub{}},
		"pub": {KV: &fakeKV{}, Pub: &fakePub{err: errors.New("no responders")}},
	} {
		// Either side failing is a ProjectionError; after the commit it
		// leaves the decision standing and the intent unprojected.
		var pe *policy.ProjectionError
		if err := p.Project(t.Context(), st.byID[d.IntentID]); !errors.As(err, &pe) || pe.HTTPStatus() != 503 {
			t.Fatalf("%s: %v", name, err)
		}
		s2, st2, _ := newService(newRig())
		s2.Projector = p
		d2, _, err := submit(t, s2, baseRequest())
		if ids, _ := st2.Unprojected(t.Context(), 10); err != nil || len(st2.byID) != 1 || len(ids) != 1 || ids[0] != d2.IntentID {
			t.Fatalf("%s: %v, %d stored, unprojected %v", name, err, len(st2.byID), ids)
		}
	}
}

// WP-9: intent/state/v1 carries the Annex IV category, class label and UA
// registration for the F3411 flight details; an undeclared class label
// or registration is null, a declared one is carried (E-01 pair).
func TestStateCarriesTheEUClassification(t *testing.T) {
	r := &Record{Request: Request{Category: CategoryCertified, ClassLabel: "C2", UARegistration: "GEO-TEST-UA-1"}}
	b := StateOf(r)
	if b.Category != CategoryCertified || b.ClassLabel == nil || *b.ClassLabel != "C2" || b.UARegistration == nil || *b.UARegistration != "GEO-TEST-UA-1" {
		t.Fatalf("declared: %+v", b)
	}
	raw, err := json.Marshal(StateOf(&Record{Request: Request{Category: CategoryOpen}}))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["category"] != CategoryOpen || m["class_label"] != nil || m["ua_registration"] != nil {
		t.Fatalf("undeclared: %s", raw)
	}
	if _, ok := m["class_label"]; !ok {
		t.Fatalf("class_label absent rather than null: %s", raw)
	}
}

type fakeNotices struct {
	err  error
	sent []string
}

func (f *fakeNotices) PublishNotice(_ context.Context, r *Record, n *Notice) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, r.ID+"@"+n.AlertID)
	return nil
}

// The version a re-check wrote publishes its notice before the
// projection; a later version does not publish it again; a notice that
// cannot be published leaves the version unprojected (a
// ProjectionError, republished by the sweep) and changes no KV (E-01
// pairs).
func TestBusProjectorPublishesTheNoticeOfItsVersion(t *testing.T) {
	n := Notice{Cause: CauseRestriction, Ref: "TRS001", Decision: RecheckWithdrawn, Version: 2, AlertID: "4d6f0f7e-8d7c-4c1a-9e2b-3a4b5c6d7e82"}
	raw, _ := json.Marshal(n)
	r := &Record{ID: "8d0e7b51-3c1e-4a5f-9a43-0b6f4c2a7e01", Version: 2, LocalState: StateWithdrawn, UpdateRequired: raw}
	kv, pub, notes := &fakeKV{}, &fakePub{}, &fakeNotices{}
	p := BusProjector{KV: kv, Pub: pub, Notices: notes}
	if err := p.Project(t.Context(), r); err != nil || len(notes.sent) != 1 || len(kv.dels) != 1 {
		t.Fatalf("%v %v %v", err, notes.sent, kv.dels)
	}
	r.Version = 3
	if err := p.Project(t.Context(), r); err != nil || len(notes.sent) != 1 {
		t.Fatalf("a later version published the notice again: %v %v", err, notes.sent)
	}
	r.Version = 2
	notes.err = errors.New("bus down")
	kv2 := &fakeKV{}
	err := BusProjector{KV: kv2, Pub: pub, Notices: notes}.Project(t.Context(), r)
	var pe *policy.ProjectionError
	if !errors.As(err, &pe) || pe.Bucket != "alrt.v1" || len(kv2.dels) != 0 {
		t.Fatalf("%v %v", err, kv2.dels)
	}
	// A WP-7 flag carries no notice: nothing to publish.
	r.UpdateRequired = json.RawMessage(`{"by_intent_id":"x"}`)
	if NoticeOf(r) != nil {
		t.Fatal("a flag read as a notice")
	}
}
