package convert_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/f3548"

	"github.com/rootxkit/uspace-ussp/internal/stdapi/convert"
	stdf3411 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3411"
	stdf3548 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3548"
)

const coreModule = "github.com/rootxkit/uspace-core"

// coreExamples is the directory of core's example messages for std
// (f3411 or f3548), in the module cache at the version go.mod pins. The
// examples are core's, assembled from the same pinned OpenAPI files; a
// missing directory fails the test rather than skipping it.
func coreExamples(t *testing.T, std string) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", coreModule).Output()
	if err != nil {
		t.Fatalf("go list -m %s: %v", coreModule, err)
	}
	dir := filepath.Join(strings.TrimSpace(string(out)), std, "testdata", "examples")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("core's examples: %v", err)
	}
	return dir
}

func read(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// generic decodes raw as plain JSON values, for a by-value comparison.
func generic(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%v in %s", err, raw)
	}
	return v
}

func sameJSON(t *testing.T, name string, want, got []byte) {
	t.Helper()
	if !reflect.DeepEqual(generic(t, want), generic(t, got)) {
		t.Errorf("%s: not equal by value\nwant %s\ngot  %s", name, want, got)
	}
}

// visit writes a strict-server response object as the generated server
// would and returns the recorded response.
func visit(t *testing.T, fn func(http.ResponseWriter) error) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := fn(rec); err != nil {
		t.Fatal(err)
	}
	return rec.Result()
}

func decodeInto[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields() // a member the types do not know fails here
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// requestBody returns the body of a request the generated client built.
func requestBody(t *testing.T, r *http.Request, err error) []byte {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	b := new(bytes.Buffer)
	if _, err := b.ReadFrom(r.Body); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Every example message core publishes for F3411 goes through convert
// and through the generated server and client, and comes back equal by
// value. A new core example fails until it is added here.
func TestF3411ExamplesRoundTrip(t *testing.T) {
	dir := coreExamples(t, "f3411")
	cases := map[string]func(t *testing.T, raw []byte) []byte{
		"rid_flight.json":                rid,
		"rid_flight_special_values.json": rid,
		"rid_flight_max_speed.json":      rid,
		"rid_flight_operating_area.json": rid,
		"get_flights_response.json": func(t *testing.T, raw []byte) []byte {
			in, err := convert.GetFlightsResponseFromWire(raw)
			if err != nil {
				t.Fatal(err)
			}
			// The USS-side server answers with it, a peer DP's client parses it.
			res := visit(t, stdf3411.SearchFlights200JSONResponse(*in).VisitSearchFlightsResponse)
			reply, err := stdf3411.ParseSearchFlightsReply(res)
			if err != nil || reply.JSON200 == nil {
				t.Fatalf("parse: %v", err)
			}
			out, err := convert.GetFlightsResponseToWire(reply.JSON200)
			if err != nil {
				t.Fatal(err)
			}
			return out
		},
		"get_flight_details_response.json": func(t *testing.T, raw []byte) []byte {
			in := decodeInto[f3411.GetFlightDetailsResponse](t, raw)
			res := visit(t, stdf3411.GetFlightDetails200JSONResponse(in).VisitGetFlightDetailsResponse)
			reply, err := stdf3411.ParseGetFlightDetailsReply(res)
			if err != nil || reply.JSON200 == nil {
				t.Fatalf("parse: %v", err)
			}
			return marshal(t, reply.JSON200)
		},
		"identification_service_area.json": func(t *testing.T, raw []byte) []byte {
			isa := decodeInto[f3411.IdentificationServiceArea](t, raw)
			// The DSS's answer to a PUT, parsed by the generated client.
			body := marshal(t, f3411.PutIdentificationServiceAreaResponse{ServiceArea: isa})
			res := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
				Body: httpBody(body)}
			reply, err := stdf3411.ParseCreateIdentificationServiceAreaReply(res)
			if err != nil || reply.JSON200 == nil {
				t.Fatalf("parse: %v", err)
			}
			return marshal(t, reply.JSON200.ServiceArea)
		},
		"put_isa_parameters.json": func(t *testing.T, raw []byte) []byte {
			in := decodeInto[f3411.CreateIdentificationServiceAreaParameters](t, raw)
			if _, err := convert.RIDVolume4DEnvelope(in.Extents); err != nil {
				t.Fatalf("envelope: %v", err)
			}
			req, err := stdf3411.NewCreateIdentificationServiceAreaRequest("https://dss.test/rid/v2", "03e5572a-f733-49af-bc14-8a18bd53ee39", in)
			return requestBody(t, req, err)
		},
		"subscription.json": func(t *testing.T, raw []byte) []byte {
			sub := decodeInto[f3411.Subscription](t, raw)
			body := marshal(t, f3411.GetSubscriptionResponse{Subscription: sub})
			res := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: httpBody(body)}
			reply, err := stdf3411.ParseGetSubscriptionReply(res)
			if err != nil || reply.JSON200 == nil {
				t.Fatalf("parse: %v", err)
			}
			return marshal(t, reply.JSON200.Subscription)
		},
	}
	runExamples(t, dir, cases)
}

func rid(t *testing.T, raw []byte) []byte {
	t.Helper()
	f, err := convert.RIDFlightFromWire(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := convert.RIDFlightToWire(f)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The same for F3548.
func TestF3548ExamplesRoundTrip(t *testing.T) {
	dir := coreExamples(t, "f3548")
	intent := func(t *testing.T, raw []byte) []byte {
		oi, err := convert.OperationalIntentFromWire(raw)
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range *oi.Details.Volumes {
			if _, err := convert.Volume4DEnvelope(v); err != nil {
				t.Fatalf("volume %d: %v", i, err)
			}
		}
		// Served by our USS server, read by a peer's generated client
		// and back through convert.
		res := visit(t, stdf3548.GetOperationalIntentDetails200JSONResponse{OperationalIntent: *oi}.VisitGetOperationalIntentDetailsResponse)
		reply, err := stdf3548.ParseGetOperationalIntentDetailsReply(res)
		if err != nil || reply.JSON200 == nil {
			t.Fatalf("parse: %v", err)
		}
		details, err := convert.OperationalIntentDetailsFromWire(reply.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(details, reply.JSON200) {
			t.Error("convert and the generated client read the details differently")
		}
		out, err := convert.OperationalIntentToWire(&details.OperationalIntent)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	cases := map[string]func(t *testing.T, raw []byte) []byte{
		"operational_intent.json":                  intent,
		"operational_intent_activated_circle.json": intent,
		"operational_intent_contingent.json":       intent,
		"operational_intent_reference_nonconforming.json": func(t *testing.T, raw []byte) []byte {
			in := decodeInto[f3548.GetOperationalIntentReferenceResponse](t, raw)
			res := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: httpBody(marshal(t, in))}
			reply, err := stdf3548.ParseGetOperationalIntentReferenceReply(res)
			if err != nil || reply.JSON200 == nil {
				t.Fatalf("parse: %v", err)
			}
			return marshal(t, reply.JSON200)
		},
		"constraint.json": func(t *testing.T, raw []byte) []byte {
			in := decodeInto[f3548.GetConstraintDetailsResponse](t, raw)
			res := visit(t, stdf3548.GetConstraintDetails200JSONResponse(in).VisitGetConstraintDetailsResponse)
			// GetConstraintDetails is a USS-side operation; the client
			// does not call it, so the body is read back as the type.
			b := new(bytes.Buffer)
			if _, err := b.ReadFrom(res.Body); err != nil {
				t.Fatal(err)
			}
			return marshal(t, decodeInto[f3548.GetConstraintDetailsResponse](t, b.Bytes()))
		},
		"operational_intent_telemetry.json": func(t *testing.T, raw []byte) []byte {
			in := decodeInto[f3548.GetOperationalIntentTelemetryResponse](t, raw)
			res := visit(t, stdf3548.GetOperationalIntentTelemetry200JSONResponse(in).VisitGetOperationalIntentTelemetryResponse)
			b := new(bytes.Buffer)
			if _, err := b.ReadFrom(res.Body); err != nil {
				t.Fatal(err)
			}
			return marshal(t, decodeInto[f3548.GetOperationalIntentTelemetryResponse](t, b.Bytes()))
		},
		"error_response.json": func(t *testing.T, raw []byte) []byte {
			in := decodeInto[f3548.ErrorResponse](t, raw)
			res := visit(t, stdf3548.GetOperationalIntentDetails404JSONResponse(in).VisitGetOperationalIntentDetailsResponse)
			reply, err := stdf3548.ParseGetOperationalIntentDetailsReply(res)
			if err != nil || reply.JSON404 == nil {
				t.Fatalf("parse: %v", err)
			}
			return marshal(t, reply.JSON404)
		},
	}
	runExamples(t, dir, cases)
}

func runExamples(t *testing.T, dir string, cases map[string]func(*testing.T, []byte) []byte) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		seen = append(seen, name)
		fn, ok := cases[name]
		if !ok {
			t.Errorf("core example %s has no round-trip case here", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			raw := read(t, dir, name)
			sameJSON(t, name, raw, fn(t, raw))
		})
	}
	for name := range cases {
		if !slices.Contains(seen, name) {
			t.Errorf("case %s names no core example", name)
		}
	}
}

func httpBody(b []byte) *readCloser { return &readCloser{bytes.NewReader(b)} }

type readCloser struct{ *bytes.Reader }

func (readCloser) Close() error { return nil }

// mutate decodes raw, applies fn to the generic value and re-encodes it.
func mutate(t *testing.T, raw []byte, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	return marshal(t, m)
}

func fieldOf(t *testing.T, err error) string {
	t.Helper()
	var fe *core.FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("%v is not a *core.FieldError", err)
	}
	return fe.Field
}

// Refusals are core's, and each has its accepted twin: the same message
// with the one member put right passes.
func TestRIDFlightRefusedAndAccepted(t *testing.T) {
	raw := read(t, coreExamples(t, "f3411"), "rid_flight.json")
	setLat := func(lat float64) []byte {
		return mutate(t, raw, func(m map[string]any) {
			m["current_state"].(map[string]any)["position"].(map[string]any)["lat"] = lat
		})
	}
	if _, err := convert.RIDFlightFromWire(setLat(34.1)); err != nil {
		t.Fatalf("accept: %v", err)
	}
	_, err := convert.RIDFlightFromWire(setLat(91))
	if got := fieldOf(t, err); got != "flight.current_state.position.lat" {
		t.Errorf("refused at %q", got)
	}
	if _, err := convert.RIDFlightFromWire(bytes.Repeat([]byte(" "), f3411.MaxMessageBytes+1)); err == nil {
		t.Error("a message over the bound accepted")
	}

	// The writer refuses what core would refuse, and writes what it accepts.
	f, err := convert.RIDFlightFromWire(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := convert.RIDFlightToWire(f); err != nil {
		t.Fatalf("write: %v", err)
	}
	bad := 91.0
	f.CurrentState.Position.Lat = &bad
	if _, err := convert.RIDFlightToWire(f); err == nil {
		t.Error("a flight core refuses was written")
	}
	if _, err := convert.RIDFlightToWire(nil); err == nil {
		t.Error("nil written")
	}

	resp := read(t, coreExamples(t, "f3411"), "get_flights_response.json")
	r, err := convert.GetFlightsResponseFromWire(resp)
	if err != nil {
		t.Fatal(err)
	}
	badLng := 181.0
	(*r.Flights)[0].CurrentState.Position.Lng = &badLng
	if _, err := convert.GetFlightsResponseToWire(r); err == nil {
		t.Error("a response core refuses was written")
	}
	if _, err := convert.GetFlightsResponseToWire(nil); err == nil {
		t.Error("nil written")
	}
}

func TestOperationalIntentRefusedAndAccepted(t *testing.T) {
	raw := read(t, coreExamples(t, "f3548"), "operational_intent.json")
	wrap := func(member []byte) []byte {
		return []byte(`{"operational_intent":` + string(member) + `}`)
	}
	if _, err := convert.OperationalIntentDetailsFromWire(wrap(raw)); err != nil {
		t.Fatalf("accept: %v", err)
	}
	bad := mutate(t, raw, func(m map[string]any) { m["reference"].(map[string]any)["state"] = "Hovering" })
	_, err := convert.OperationalIntentFromWire(bad)
	if got := fieldOf(t, err); got != "operational_intent.reference.state" {
		t.Errorf("refused at %q", got)
	}
	_, err = convert.OperationalIntentDetailsFromWire(wrap(bad))
	if got := fieldOf(t, err); got != "response.operational_intent.reference.state" {
		t.Errorf("details refused at %q", got)
	}
	for name, body := range map[string][]byte{
		"no member":   []byte(`{}`),
		"null member": []byte(`{"operational_intent":null}`),
		"not object":  []byte(`[1]`),
		"not JSON":    []byte(`{`),
		"over bound":  bytes.Repeat([]byte(" "), f3548.MaxMessageBytes+1),
	} {
		if _, err := convert.OperationalIntentDetailsFromWire(body); err == nil {
			t.Errorf("%s accepted", name)
		} else {
			fieldOf(t, err)
		}
	}

	oi, err := convert.OperationalIntentFromWire(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := convert.OperationalIntentDetailsToWire(&f3548.GetOperationalIntentDetailsResponse{OperationalIntent: *oi}); err != nil {
		t.Fatalf("write details: %v", err)
	}
	oi.Reference.State = "Hovering"
	if _, err := convert.OperationalIntentToWire(oi); err == nil {
		t.Error("an intent core refuses was written")
	}
	if _, err := convert.OperationalIntentDetailsToWire(&f3548.GetOperationalIntentDetailsResponse{OperationalIntent: *oi}); err == nil {
		t.Error("details core refuses were written")
	}
	if _, err := convert.OperationalIntentToWire(nil); err == nil {
		t.Error("nil written")
	}
	if _, err := convert.OperationalIntentDetailsToWire(nil); err == nil {
		t.Error("nil written")
	}
}

// An envelope is core's; a volume it cannot bound is refused, never an
// empty box.
func TestEnvelopesRefusedAndAccepted(t *testing.T) {
	oi, err := convert.OperationalIntentFromWire(read(t, coreExamples(t, "f3548"), "operational_intent.json"))
	if err != nil {
		t.Fatal(err)
	}
	v := (*oi.Details.Volumes)[0]
	env, err := convert.Volume4DEnvelope(v)
	if err != nil {
		t.Fatal(err)
	}
	if env.BBox.MinLat > 34.123 || env.BBox.MaxLat < 34.133 || env.Start.IsZero() || !env.End.After(env.Start) {
		t.Errorf("envelope %+v", env)
	}
	v.Volume.OutlinePolygon = nil
	if _, err := convert.Volume4DEnvelope(v); err == nil {
		t.Error("a volume without an outline bounded")
	}

	var isa f3411.CreateIdentificationServiceAreaParameters
	if err := json.Unmarshal(read(t, coreExamples(t, "f3411"), "put_isa_parameters.json"), &isa); err != nil {
		t.Fatal(err)
	}
	if _, err := convert.RIDVolume4DEnvelope(isa.Extents); err != nil {
		t.Fatal(err)
	}
	isa.Extents.Volume.OutlinePolygon, isa.Extents.Volume.OutlineCircle = nil, nil
	if _, err := convert.RIDVolume4DEnvelope(isa.Extents); err == nil {
		t.Error("an ISA without an outline bounded")
	}
}
