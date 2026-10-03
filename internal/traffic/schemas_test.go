package traffic

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

const schemaBase = "https://schemas.uspace.ge/"

// schemas compiles every schema under schemas/ (offline).
func schemas(t *testing.T) map[string]*jsonschema.Schema {
	t.Helper()
	root := filepath.Join("..", "..", "schemas")
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	var names []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "schema.json" {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		name := filepath.ToSlash(rel)
		names = append(names, name)
		return c.AddResource(schemaBase+name+".json", doc)
	})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*jsonschema.Schema{}
	for _, n := range names {
		s, err := c.Compile(schemaBase + n + ".json")
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		out[n] = s
	}
	return out
}

// validate checks v (marshalled) against the schema name.
func validate(t *testing.T, ss map[string]*jsonschema.Schema, name string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := ss[name].Validate(inst); err != nil {
		t.Fatalf("%s does not validate: %v\n%s", name, err, raw)
	}
}

// Every alert the CPA path publishes validates against alert/v1: raised,
// updated, cleared resolved with its clearing numbers, the carried one,
// a manned peer with no vertical, the not_reconfirmed clear.
func TestAlertMessagesValidateAgainstTheSchema(t *testing.T) {
	ss := schemas(t)
	r := newRig(t, nil, "m1")
	r.headOn(600, 40)
	states := map[string]bool{}
	for _, b := range r.msgs {
		states[b.State] = true
		validate(t, ss, "alert/v1", &AlertMessage{Envelope: envelopeAt(t0), Body: b})
	}
	if !states[AlertRaised] || !states[AlertUpdated] || !states[AlertCleared] {
		t.Fatalf("states %v", states)
	}
	// Carried, then not reconfirmed (TestCarriedNotReconfirmedAfterHysteresis's run).
	store := newMemStore()
	r2 := newRig(t, store, "m1")
	b := geodesy.Destination(origin, 90, 25)
	r2.feed(r2.own(flightA, origin, 0, 0))
	r2.feed(r2.own(flightB, b, 0, 0))
	r2.tick()
	r2.drainPersist()
	r3 := newRig(t, store, "m1")
	r3.e.restore(t.Context())
	far := geodesy.Destination(origin, 90, 3000)
	for i := 0; i < 6; i++ {
		r3.feed(r3.own(flightA, origin, 0, 0))
		r3.feed(r3.own(flightB, far, 0, 0))
		r3.tick()
		r3.clk.add(time.Second)
	}
	reasons := map[string]bool{}
	for _, b := range r3.msgs {
		validate(t, ss, "alert/v1", &AlertMessage{Envelope: envelopeAt(t0), Body: b})
		if b.ClearReason != nil {
			reasons[*b.ClearReason] = true
		}
	}
	if !reasons[ClearNotReconfirmed] {
		t.Fatalf("reasons %v", reasons)
	}
}

// E-03: DecodeManned and the pinned track/manned/v1 agree on every
// example, both ways.
func TestMannedDecoderAgreesWithTheSchemaExamples(t *testing.T) {
	ss := schemas(t)
	checkExamples(t, ss, "track/manned/v1", func(raw []byte) error { _, err := DecodeManned(raw); return err })
}

// E-03: DecodeTrack and the pinned track/telemetry/v1 agree on every
// example, both ways.
func TestTrackDecoderAgreesWithTheSchemaExamples(t *testing.T) {
	ss := schemas(t)
	checkExamples(t, ss, "track/telemetry/v1", func(raw []byte) error { _, err := DecodeTrack(raw); return err })
}

func checkExamples(t *testing.T, ss map[string]*jsonschema.Schema, name string, decode func([]byte) error) {
	t.Helper()
	dir := filepath.Join("..", "..", "schemas", filepath.FromSlash(name), "examples")
	n := 0
	for _, want := range []bool{true, false} {
		d := dir
		if !want {
			d = filepath.Join(dir, "invalid")
		}
		es, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(d, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			schemaOK := ss[name].Validate(inst) == nil
			derr := decode(raw)
			if schemaOK != want || (derr == nil) != want {
				t.Errorf("%s: schema valid %v, decoder error %v, want valid %v", e.Name(), schemaOK, derr, want)
			}
			n++
		}
	}
	if n == 0 {
		t.Fatalf("no example of %s", name)
	}
}

func envelopeAt(at time.Time) bus.Envelope { return bus.SystemEnvelope(SchemaAlert, Producer, at) }
