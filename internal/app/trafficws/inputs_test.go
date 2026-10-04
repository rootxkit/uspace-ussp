package trafficws

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/sources"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
	"github.com/rootxkit/uspace-ussp/internal/traffic"
)

// statusMsg is one src.v1 message as the WP-14 adapters publish it.
func statusMsg(t *testing.T, source string, instance *string, state string, since time.Time, detail string) []byte {
	t.Helper()
	b := sources.StatusBody{Source: source, SourceInstance: instance, State: state, Since: bus.Stamp{Time: since},
		Counters: map[string]uint64{"accepted": 0, "refused": 0}, Detail: detail}
	if state == sources.StateDisabled {
		why := "type"
		b.DisabledBy = &why
	}
	raw, err := json.Marshal(sources.Status{Envelope: bus.SystemEnvelope(sources.SchemaStatus, "ussp/manned-feed", time.Now()), Body: b})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type offGate map[string]bool

func (g offGate) Query(t string, _ *string) coresources.Decision {
	if g[t] {
		w := coresources.WhyType
		return coresources.Decision{WhyDisabled: &w}
	}
	return coresources.Decision{Enabled: true}
}

func degradedOf(h *Hub, now time.Time, input string) (traffic.Degraded, bool) {
	d, ok := h.Degraded(now)[input]
	return d, ok
}

// The manned input as its adapter says it on src.v1 (WP-14): live is
// not degraded; down is "unavailable since T" with the adapter's T;
// stale is "manned: stale since T (the ANSP's time)", not unavailable;
// switched off says so; a status not refreshed falls back to the
// silence of the feed (E-01 both ways, SC-22).
func TestHubMannedFromTheFeedStatus(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	h := &Hub{Picture: &traffic.Picture{}, Book: &traffic.Book{}, Now: func() time.Time { return now }}
	h.Started()
	if d, ok := degradedOf(h, now, "manned"); !ok || !strings.Contains(d.Reason, "no ANSP feed track or status") {
		t.Fatalf("nothing heard: %+v", d)
	}
	h.TakeSourceStatus(statusMsg(t, traffic.SourceANSPFeed, nil, "live", now.Add(-time.Minute), "connected"))
	if d, ok := degradedOf(h, now, "manned"); ok {
		t.Fatalf("live: %+v", d)
	}
	since := now.Add(-42 * time.Second)
	h.TakeSourceStatus(statusMsg(t, traffic.SourceANSPFeed, nil, "stale", since, "the ANSP says fake-adsb-1 stale"))
	d, ok := degradedOf(h, now, "manned")
	if !ok || d.Since == nil || !d.Since.Equal(since) || !strings.HasPrefix(d.Reason, "manned: stale since 2026-10-04T11:59:18Z (the ANSP's time)") ||
		strings.Contains(d.Reason, "unavailable") {
		t.Fatalf("stale: %+v", d)
	}
	cut := now.Add(-3 * time.Second)
	h.TakeSourceStatus(statusMsg(t, traffic.SourceANSPFeed, nil, "down", cut, "unavailable: no frame"))
	if d, ok := degradedOf(h, now, "manned"); !ok || !d.Since.Equal(cut) || !strings.HasPrefix(d.Reason, "manned traffic unavailable since 2026-10-04T11:59:57Z") {
		t.Fatalf("down: %+v", d)
	}
	h.TakeSourceStatus(statusMsg(t, traffic.SourceANSPFeed, nil, "disabled", cut, ""))
	if d, ok := degradedOf(h, now, "manned"); !ok || !strings.Contains(d.Reason, "switched off since") {
		t.Fatalf("disabled: %+v", d)
	}
	// The status not refreshed for over 10 s: the feed's silence shows.
	if d, ok := degradedOf(h, now.Add(time.Minute), "manned"); !ok || !strings.Contains(d.Reason, "sent nothing for") {
		t.Fatalf("silent: %+v", d)
	}
}

// e-conspicuity (02 F4, a required input outside ATC service): no
// receiver is shown as such; down and stale with their time; live is
// not degraded; switched off says so.
func TestHubEconspicuity(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	g := offGate{}
	h := &Hub{Picture: &traffic.Picture{}, Book: &traffic.Book{}, Now: func() time.Time { return now }, Gate: g}
	h.Started()
	if d, ok := degradedOf(h, now, "econspicuity"); !ok || !strings.Contains(d.Reason, "no e-conspicuity receiver status") {
		t.Fatalf("none: %+v", d)
	}
	rx := "rx-1"
	h.TakeSourceStatus(statusMsg(t, traffic.SourceAdsbRx, &rx, "live", now, "receiving"))
	if d, ok := degradedOf(h, now, "econspicuity"); ok {
		t.Fatalf("live: %+v", d)
	}
	h.TakeSourceStatus(statusMsg(t, traffic.SourceAdsbRx, &rx, "down", now.Add(-5*time.Second), "no answer"))
	if d, ok := degradedOf(h, now, "econspicuity"); !ok || !strings.HasPrefix(d.Reason, "e-conspicuity unavailable since") {
		t.Fatalf("down: %+v", d)
	}
	h.TakeSourceStatus(statusMsg(t, traffic.SourceAdsbRx, &rx, "stale", now.Add(-5*time.Second), "frozen"))
	if d, ok := degradedOf(h, now, "econspicuity"); !ok || !strings.HasPrefix(d.Reason, "e-conspicuity stale since") {
		t.Fatalf("stale: %+v", d)
	}
	g[traffic.SourceAdsbRx] = true
	if d, ok := degradedOf(h, now, "econspicuity"); !ok || !strings.Contains(d.Reason, "switched off") {
		t.Fatalf("off: %+v", d)
	}
}

// Peers: no Display Provider status is unavailable; live is not
// degraded unless a peer is down (then counted, since its first
// failure); discovery stale says why; network_rid switched off says
// so. A down peer's flights are marked peer_unavailable in the product
// and age out after peer_unavailable_s (E-01 pair with a live peer).
func TestHubPeers(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	g := offGate{}
	h := &Hub{Picture: &traffic.Picture{}, Book: &traffic.Book{}, Now: func() time.Time { return now }, Gate: g,
		Policy: func() policy.Record { return policy.Record{Values: policy.Defaults()} }}
	h.Started()
	if d, ok := degradedOf(h, now, "peers"); !ok || !strings.Contains(d.Reason, "no network_rid status") {
		t.Fatalf("none: %+v", d)
	}
	peer := "https://peer.example/uss"
	h.TakeSourceStatus(statusMsg(t, traffic.SourceNetworkRID, nil, "live", now, "1 peers"))
	h.TakeSourceStatus(statusMsg(t, traffic.SourceNetworkRID, &peer, "live", now, "answering"))
	if d, ok := degradedOf(h, now, "peers"); ok {
		t.Fatalf("live: %+v", d)
	}
	// A peer flight, last reported 2 s ago.
	ts := now.Add(-2 * time.Second)
	id := "peer-flight-1"
	tr := &telemetry.Track{Envelope: bus.NewEnvelope(telemetry.SchemaTrack, "ussp/peers", core.Times{TS: &ts, RxTS: ts, CapturedAt: ts, Source: core.TimeProvider}),
		Body: telemetry.TrackBody{TrackID: id, Trust: core.TrustProvider, Source: traffic.SourceNetworkRID, SourceInstance: peer,
			Position: telemetry.Position{Lat: 41.75, Lng: 44.85}, AltSource: core.AltNone,
			Identification: core.Identification{Status: core.IdentUnidentified, Reason: core.ReasonNoSerial, Basis: core.BasisAsBroadcast}}}
	raw, _ := json.Marshal(tr)
	h.TakeTrack(traffic.NSPeer, raw)
	area := traffic.Area{Boxes: boxesOf([4]float64{44.8, 41.7, 44.9, 41.8})}
	sel := h.Picture.Tracks(area, now, policy.Defaults(), h)
	if len(sel) != 1 || sel[0].Track.PeerUnavailable {
		t.Fatalf("a live peer's flight: %+v", sel)
	}
	h.TakeSourceStatus(statusMsg(t, traffic.SourceNetworkRID, &peer, "down", now.Add(-time.Second), "503"))
	sel = h.Picture.Tracks(area, now, policy.Defaults(), h)
	if len(sel) != 1 || !sel[0].Track.PeerUnavailable {
		t.Fatalf("a down peer's flight: %+v", sel)
	}
	if d, ok := degradedOf(h, now, "peers"); !ok || !strings.HasPrefix(d.Reason, "1 peer USSPs do not answer") {
		t.Fatalf("one down: %+v", d)
	}
	v := policy.Defaults()
	v.PeerUnavailableS = 1
	if sel := h.Picture.Tracks(area, now, v, h); len(sel) != 0 {
		t.Fatalf("aged out after peer_unavailable_s: %+v", sel)
	}
	h.TakeSourceStatus(statusMsg(t, traffic.SourceNetworkRID, nil, "stale", now.Add(-time.Minute), "the DSS cannot be searched"))
	if d, ok := degradedOf(h, now, "peers"); !ok || !strings.Contains(d.Reason, "the DSS cannot be searched") {
		t.Fatalf("stale: %+v", d)
	}
	g[traffic.SourceNetworkRID] = true
	if d, ok := degradedOf(h, now, "peers"); !ok || !strings.Contains(d.Reason, "switched off") {
		t.Fatalf("off: %+v", d)
	}
	if clipDetail(strings.Repeat("ა", 200)) == "" || len(clipDetail(strings.Repeat("x", 400))) != 300 || clipDetail("a\xffb") != "ab" {
		t.Fatal("clipDetail")
	}
}
