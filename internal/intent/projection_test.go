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
