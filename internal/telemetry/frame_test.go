package telemetry

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaBase = "https://schemas.uspace.ge/"

// schemas compiles every schema under schemas/ (owned and pinned
// copies) offline, by name.
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
	s, ok := ss[name]
	if !ok {
		t.Fatalf("no schema %s", name)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("%s does not validate: %v\n%s", name, err, raw)
	}
}

// E-03: the decoder and schemas/telemetry/v1 agree on every example, in
// both directions: each valid example decodes, each invalid one (trust
// simulated and source sitl among them, 06 T11) is refused by both.
func TestDecoderAgreesWithTheSchemaExamples(t *testing.T) {
	ss := schemas(t)
	sch := ss["telemetry/v1"]
	dir := filepath.Join("..", "..", "schemas", "telemetry", "v1", "examples")
	valid, invalid := 0, 0
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
			schemaOK := sch.Validate(inst) == nil
			_, derr := DecodeFrame(raw, "")
			if schemaOK != want || (derr == nil) != want {
				t.Errorf("%s: schema %v decoder %v, want %v", e.Name(), schemaOK, derr, want)
			}
			if want {
				valid++
			} else {
				invalid++
			}
		}
	}
	if valid < 4 || invalid < 10 {
		t.Fatalf("%d valid and %d invalid examples", valid, invalid)
	}
}

// The decoder names each refused field and reads what the schema allows.
func TestDecodeFrameFields(t *testing.T) {
	f, err := DecodeFrame([]byte(sampleJSON(`"intent_id":"`+intent1+`","backlog":true,"end":true,"alt_pressure_m":null,"operator_position":{"lat":41.7,"lng":44.8,"alt_wgs84_m":500},`)), "")
	if err != nil {
		t.Fatal(err)
	}
	if f.IntentID == nil || *f.IntentID != intent1 || !f.Backlog || !f.End || f.OperatorPosition == nil || *f.OperatorPosition.AltWGS84M != 500 ||
		f.Seq != 1 || f.Serial != snA || !f.TS.Equal(t0) {
		t.Fatalf("%+v", f)
	}
	for name, c := range map[string]struct{ json, field string }{
		"seq as string":     {strings.Replace(sampleJSON(""), `"seq":1`, `"seq":"1"`, 1), "seq"},
		"seq fraction":      {strings.Replace(sampleJSON(""), `"seq":1`, `"seq":1.5`, 1), "seq"},
		"no ts":             {strings.Replace(sampleJSON(""), `"ts":"2026-10-02T09:00:00.000Z",`, ``, 1), "ts"},
		"ts offset":         {strings.Replace(sampleJSON(""), `00.000Z"`, `00.000+04:00"`, 1), "ts"},
		"ts garbage":        {strings.Replace(sampleJSON(""), `"2026-10-02T09:00:00.000Z"`, `"yesterdayZ"`, 1), "ts"},
		"serial space":      {strings.Replace(sampleJSON(""), `"TEST-SN-A"`, `"TEST SN"`, 1), "serial"},
		"speed negative":    {strings.Replace(sampleJSON(""), `"speed_ms":8`, `"speed_ms":-1`, 1), "speed_ms"},
		"speed string":      {strings.Replace(sampleJSON(""), `"speed_ms":8`, `"speed_ms":"8"`, 1), "speed_ms"},
		"height without":    {strings.Replace(sampleJSON(""), `"height_ref":"TakeoffLocation"`, `"height_ref":null`, 1), "height_ref"},
		"bad height ref":    {strings.Replace(sampleJSON(""), `"TakeoffLocation"`, `"Sea"`, 1), "height_ref"},
		"no height ref":     {strings.Replace(sampleJSON(""), `"height_ref":"TakeoffLocation",`, ``, 1), "height_ref"},
		"status":            {strings.Replace(sampleJSON(""), `"Airborne"`, `"Up"`, 1), "status"},
		"accuracy_h":        {strings.Replace(sampleJSON(""), `"HA3m"`, `"HA2m"`, 1), "accuracy_h"},
		"accuracy_v":        {strings.Replace(sampleJSON(""), `"VA10m"`, `"VA2m"`, 1), "accuracy_v"},
		"position missing":  {strings.Replace(sampleJSON(""), `"position":{"lat":41.7151,"lng":44.8271},`, ``, 1), "position"},
		"position no lng":   {strings.Replace(sampleJSON(""), `,"lng":44.8271`, ``, 1), "position"},
		"position alt":      {strings.Replace(sampleJSON(""), `"lng":44.8271}`, `"lng":44.8271,"alt_wgs84_m":1}`, 1), "position"},
		"position range":    {strings.Replace(sampleJSON(""), `"lng":44.8271`, `"lng":190`, 1), "position"},
		"operator position": {sampleJSON(`"operator_position":{"lat":91,"lng":0},`), "operator_position"},
		"operator member":   {sampleJSON(`"operator_position":{"lat":1,"lng":0,"x":1},`), "operator_position"},
		"intent upper case": {sampleJSON(`"intent_id":"8C1F3F2E-7D0E-4A8B-9A51-0E4B7D6F2C11",`), "intent_id"},
		"no emergency":      {strings.Replace(sampleJSON(""), `"emergency":false,`, ``, 1), "emergency"},
		"no status":         {strings.Replace(sampleJSON(""), `"status":"Airborne",`, ``, 1), "status"},
		"no accuracy":       {strings.Replace(sampleJSON(""), `"accuracy_h":"HA3m",`, ``, 1), "accuracy_h"},
		"no accuracy v":     {strings.Replace(sampleJSON(""), `"accuracy_v":"VA10m",`, ``, 1), "accuracy_v"},
		"no serial":         {strings.Replace(sampleJSON(""), `"serial":"TEST-SN-A",`, ``, 1), "serial"},
		"no seq":            {strings.Replace(sampleJSON(""), `"seq":1,`, ``, 1), "seq"},
		"no altitude":       {strings.Replace(sampleJSON(""), `"alt_wgs84_m":650,`, ``, 1), "alt_wgs84_m"},
	} {
		_, err := DecodeFrame([]byte(c.json), "body.")
		if err == nil || !strings.Contains(err.Error(), "body."+c.field) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, bad := range []string{`[]`, `{"ts":1}`, sampleJSON("") + `{}`, `{`} {
		if _, err := DecodeFrame([]byte(bad), ""); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestDecodeMessageShapes(t *testing.T) {
	for name, msg := range map[string]string{
		"not json":       `nope`,
		"no schema":      `{"body":` + sampleJSON("") + `}`,
		"other schema":   `{"schema":"track/telemetry/v1","body":` + sampleJSON("") + `}`,
		"body not obj":   `{"schema":"telemetry/v1","body":[1]}`,
		"no body":        `{"schema":"telemetry/v1"}`,
		"invalid sample": `{"schema":"telemetry/v1","body":{}}`,
	} {
		if _, err := DecodeMessage([]byte(msg)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestDecodeBatchShapes(t *testing.T) {
	b, err := DecodeBatch([]byte(`{"sent_at":"2026-10-02T09:00:01Z","frames":[` + sampleJSON("") + `,{"serial":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Frames) != 1 || b.Index[0] != 0 || b.Invalid[1] == nil || b.SentAt == nil || !b.SentAt.Equal(t0.Add(1e9)) {
		t.Fatalf("%+v", b)
	}
	if !strings.Contains(b.Invalid[1].Error(), "frames[1].") {
		t.Fatalf("index not named: %v", b.Invalid[1])
	}
	many := `{"frames":[` + strings.Repeat(`{},`, MaxBatchFrames) + `{}]}`
	for name, body := range map[string]string{
		"not json":     `x`,
		"no frames":    `{}`,
		"empty":        `{"frames":[]}`,
		"unknown":      `{"frames":[` + sampleJSON("") + `],"x":1}`,
		"trailing":     `{"frames":[` + sampleJSON("") + `]}{}`,
		"too many":     many,
		"sent_at zone": `{"sent_at":"2026-10-02T13:00:00+04:00","frames":[` + sampleJSON("") + `]}`,
		"too long":     strings.Repeat(" ", MaxBatchBytes+1),
	} {
		if _, err := DecodeBatch([]byte(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Nothing panics on any input (CLAUDE.md: no panics on untrusted input).
func FuzzDecodeFrame(f *testing.F) {
	f.Add([]byte(sampleJSON("")))
	f.Add([]byte(`{"schema":"telemetry/v1","body":` + sampleJSON("") + `}`))
	f.Add([]byte(`{"frames":[` + sampleJSON("") + `]}`))
	f.Add([]byte(`{"seq":1e400,"position":{"lat":"x"}}`))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = DecodeFrame(data, "")
		_, _ = DecodeMessage(data)
		_, _ = DecodeBatch(data)
	})
}
