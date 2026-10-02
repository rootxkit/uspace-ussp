package ridsp

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/f3411"
)

// ParseView never panics, and a view it takes is a box within the WGS84
// ranges whose corners it read, with a diagonal that is a distance (not
// NaN, not negative; +Inf when the geodesic cannot be computed).
func FuzzParseView(f *testing.F) {
	for _, s := range []string{
		"41.7,44.8,41.71,44.81", "-90,-180,90,180", "0,0,0,0", " 41.7 , 44.8 ,41.71,44.81",
		"41.7,44.8,41.71", "41.7,44.8,41.71,44.81,1", "NaN,0,0,0", "Inf,0,0,0", "1e400,0,0,0",
		"0x1p-2,0,0,0", "90.0000001,0,0,0", "0,180,0,-180", "-0,-0,0,0", "", ",,,", strings.Repeat("1", MaxViewBytes+1),
		"89.9999999,0,-89.9999999,179.9999999",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		box, d, err := ParseView(v)
		if err != nil {
			return
		}
		if len(v) > MaxViewBytes {
			t.Fatalf("a view of %d bytes taken", len(v))
		}
		for _, c := range []float64{box.MinLat, box.MaxLat, box.MinLon, box.MaxLon} {
			if math.IsNaN(c) || math.IsInf(c, 0) {
				t.Fatalf("box %+v", box)
			}
		}
		if box.MinLat > box.MaxLat || box.MinLon > box.MaxLon || box.MinLat < -90 || box.MaxLat > 90 || box.MinLon < -180 || box.MaxLon > 180 {
			t.Fatalf("box %+v from %q", box, v)
		}
		if math.IsNaN(d) || d < 0 {
			t.Fatalf("diagonal %v from %q", d, v)
		}
	})
}

// DecodeTrack never panics; a track it serves has no error, a version 4
// flight id, a valid position and a cell, and an error is never served.
func FuzzDecodeTrack(f *testing.F) {
	f.Add(trackMsg(f, body(1, origin), t0))
	anon := body(2, origin)
	anon.FlightID = nil
	f.Add(trackMsg(f, anon, t0))
	far := body(3, origin)
	far.Position.Lat, far.Position.Lng = 90, 180
	f.Add(trackMsg(f, far, t0))
	bad := body(4, origin)
	id := "not-a-uuid"
	bad.FlightID = &id
	f.Add(trackMsg(f, bad, t0))
	for _, s := range []string{``, `{}`, `null`, `{"schema":"track/telemetry/v1"}`, `{"schema":"track/telemetry/v1","body":{"position":{"lat":1e400}}}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		s, ours, err := DecodeTrack(data)
		if err != nil {
			if ours {
				t.Fatal("an error served")
			}
			return
		}
		if !ours {
			return
		}
		if !flightIDRe.MatchString(s.FlightID()) || !s.Position().Valid() || s.Cell5 == "" {
			t.Fatalf("served %+v", s)
		}
	})
}

// checkNotification never panics; a notification it takes names the ISA
// of the path (a UUID) with 1 to 100 subscriptions whose ids are UUIDs,
// and extents only with a service_area that is the ISA of the path.
func FuzzCheckNotification(f *testing.F) {
	isa := "8c1f3f2e-7d0e-4a8b-9a51-0e4b7d6f2c11"
	start, end := t0, t0.Add(time.Hour)
	ext := f3411.Volume4D{Volume: boxVolume(geodeticBox(origin, 0.01)),
		TimeStart: &f3411.Time{Format: f3411.RFC3339, Value: start}, TimeEnd: &f3411.Time{Format: f3411.RFC3339, Value: end}}
	good := f3411.PutIdentificationServiceAreaNotificationParameters{
		Subscriptions: []f3411.SubscriptionState{{SubscriptionId: "11111111-1111-4111-8111-111111111111"}},
		ServiceArea: &f3411.IdentificationServiceArea{Id: isa, Owner: "peer", UssBaseUrl: "https://peer.test/rid", Version: "v1",
			TimeStart: f3411.Time{Format: f3411.RFC3339, Value: start}, TimeEnd: f3411.Time{Format: f3411.RFC3339, Value: end}},
		Extents: &ext,
	}
	raw, err := json.Marshal(good)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(isa, raw)
	deleted := good
	deleted.ServiceArea, deleted.Extents = nil, nil
	raw, _ = json.Marshal(deleted)
	f.Add(isa, raw)
	f.Add("not-a-uuid", raw)
	for _, s := range []string{``, `null`, `{}`, `{"subscriptions":[]}`, `{"subscriptions":[{"subscription_id":"x"}],"extents":{}}`,
		`{"subscriptions":[{"subscription_id":"11111111-1111-4111-8111-111111111111"}],"service_area":{"id":"` + isa + `"},"extents":{"volume":{}}}`} {
		f.Add(isa, []byte(s))
	}
	f.Fuzz(func(t *testing.T, id string, data []byte) {
		var b *f3411.PutIdentificationServiceAreaNotificationParameters
		if len(data) > 0 {
			b = &f3411.PutIdentificationServiceAreaNotificationParameters{}
			if json.Unmarshal(data, b) != nil {
				return
			}
		}
		if checkNotification(id, b) != "" {
			return
		}
		if b == nil || !entityUUIDRe.MatchString(id) || len(b.Subscriptions) == 0 || len(b.Subscriptions) > MaxNotificationSubscriptions {
			t.Fatalf("taken: %q %s", id, data)
		}
		for _, s := range b.Subscriptions {
			if !entityUUIDRe.MatchString(s.SubscriptionId) {
				t.Fatalf("a subscription id taken: %q", s.SubscriptionId)
			}
		}
		if b.Extents != nil && (b.ServiceArea == nil || b.ServiceArea.Id != id) {
			t.Fatalf("extents taken without the ISA: %s", data)
		}
	})
}
