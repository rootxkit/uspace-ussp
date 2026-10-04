package manned

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/rootxkit/uspace-core/core"
	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/cell"
)

// SchemaManned is track/manned/v1 (the ANSP's, pinned under
// schemas/track/manned/v1).
const SchemaManned = "track/manned/v1"

// The adapter types of this package (04 §2, the source-control types).
const (
	SourceANSPFeed = "ansp_feed"
	SourceAdsbRx   = "adsb_rx"
)

// The envelope producers of this package.
const (
	ProducerANSP = "ussp/manned-feed"
	ProducerEcon = "ussp/econspicuity"
)

// Track states (the body's state).
const (
	StateLive           = "live"
	StateStale          = "stale"
	StateSourceDisabled = "source_disabled"
)

// MaxCallsignBytes is the schema's bound on a callsign.
const MaxCallsignBytes = 8

// Position is a WGS84 position as the schema writes it.
type Position struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// LatLon is p as a core.LatLon.
func (p Position) LatLon() core.LatLon { return core.LatLon{LatDeg: p.Lat, LonDeg: p.Lng} }

// Body is the body of track/manned/v1: the members of the ANSP's schema
// this USSP carries. An ANSP body's other members (spi, relevant,
// policy_version, age_s) are read and not republished.
type Body struct {
	ICAO24         string          `json:"icao24"`
	Callsign       *string         `json:"callsign"`
	Position       Position        `json:"position"`
	AltPressureM   *float64        `json:"alt_pressure_m"`
	AltWGS84M      *float64        `json:"alt_wgs84_m"`
	GSMS           *float64        `json:"gs_ms"`
	TrackDeg       *float64        `json:"track_deg"`
	VRateMS        *float64        `json:"vrate_ms"`
	Emergency      *bool           `json:"emergency"`
	Squawk         *string         `json:"squawk,omitempty"`
	SourceClass    string          `json:"source_class"`
	Quality        json.RawMessage `json:"quality,omitempty"`
	Trust          core.Trust      `json:"trust"`
	Source         string          `json:"source"`
	SourceInstance string          `json:"source_instance"`
	State          string          `json:"state"`
}

// Track is one track/manned/v1 message.
type Track struct {
	bus.Envelope
	Body Body `json:"body"`
}

var (
	icao24Re = regexp.MustCompile(`^[0-9a-f]{6}$`)
	squawkRe = regexp.MustCompile(`^[0-7]{4}$`)
	slugRe   = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
)

var sourceClasses = map[string]bool{"ads_b": true, "mode_s": true, "ssr": true, "atm_feed": true, "ads_l": true}

var states = map[string]bool{StateLive: true, StateStale: true, StateSourceDisabled: true}

func finite(field string, v *float64) error {
	if v != nil && !core.IsFinite(*v) {
		return core.Fieldf(field, "not a finite number")
	}
	return nil
}

// Check refuses a body this USSP would not carry: an icao24 that is not
// six lower-case hex digits, a callsign over eight characters, a
// position outside WGS84, a number that is not finite, a negative
// ground speed, a track outside [0, 360), a squawk that is not four
// octal digits, an unknown source class or state, a source instance
// that is not a slug, a quality that is not an object, and a trust and
// source other than surveillance from ansp_feed or broadcast from
// adsb_rx. Every refusal names its field.
func (b *Body) Check() error {
	for _, e := range []error{finite("body.alt_pressure_m", b.AltPressureM), finite("body.alt_wgs84_m", b.AltWGS84M),
		finite("body.gs_ms", b.GSMS), finite("body.track_deg", b.TrackDeg), finite("body.vrate_ms", b.VRateMS)} {
		if e != nil {
			return e
		}
	}
	pair := (b.Trust == core.TrustSurveillance && b.Source == SourceANSPFeed) || (b.Trust == core.TrustBroadcast && b.Source == SourceAdsbRx)
	switch {
	case !icao24Re.MatchString(b.ICAO24):
		return core.Fieldf("body.icao24", "not six lower-case hex digits")
	case b.Callsign != nil && (len(*b.Callsign) > MaxCallsignBytes || !utf8.ValidString(*b.Callsign)):
		return core.Fieldf("body.callsign", "longer than %d characters", MaxCallsignBytes)
	case !b.Position.LatLon().Valid():
		return core.Fieldf("body.position", "not a WGS84 position")
	case b.GSMS != nil && *b.GSMS < 0:
		return core.Fieldf("body.gs_ms", "negative")
	case b.TrackDeg != nil && (*b.TrackDeg < 0 || *b.TrackDeg >= 360):
		return core.Fieldf("body.track_deg", "not in [0, 360)")
	case b.Squawk != nil && !squawkRe.MatchString(*b.Squawk):
		return core.Fieldf("body.squawk", "not four octal digits")
	case !sourceClasses[b.SourceClass]:
		return core.Fieldf("body.source_class", "unknown source class")
	case !states[b.State]:
		return core.Fieldf("body.state", "unknown state")
	case len(b.SourceInstance) > 64 || !slugRe.MatchString(b.SourceInstance):
		return core.Fieldf("body.source_instance", "not an adapter slug")
	case len(b.Quality) > 0 && strings.TrimSpace(string(b.Quality))[0] != '{':
		return core.Fieldf("body.quality", "not an object")
	case !pair:
		return core.Fieldf("body.trust", "%q from %q: a manned track is surveillance from ansp_feed or broadcast from adsb_rx", b.Trust, b.Source)
	}
	return nil
}

// Sink is the bus (bus.Publisher).
type Sink interface {
	Publish(ctx context.Context, subject string, m bus.Enveloped) error
}

// Gate is the source switches (internal/sources.Follower).
type Gate interface {
	Query(sourceType string, instanceID *string) coresources.Decision
}

// Own is this USSP's own flights as the echo guard reads them (PLAN §15
// Q23): EchoOf names the active own flight a manned record at position
// at is the echo of, matched by the UA registration the flight's intent
// declares against the callsign or the registration the record carries,
// and by where the flight's own track places it. A record with neither
// mark, with one no active flight declares, or away from that flight,
// is no echo.
type Own interface {
	EchoOf(icao24 string, callsign, registration *string, at core.LatLon) (flightID string, ok bool)
}

// Counters of the publication (shared by both adapters).
const (
	CounterPublished     = "manned_published"
	CounterPublishFailed = "manned_publish_failed"
	CounterEchoOwnFlight = "manned_echo_own_flight"
)

// publish sends m on man.v1.<cell3>.<cell5>.<icao24>, unless the echo
// guard recognises one of this USSP's own flights in it (counted, never
// published: the own flight's track shows it).
func publish(ctx context.Context, sink Sink, own Own, counters *core.Counters, m *Track, registration *string) (string, error) {
	if own != nil {
		if fid, ok := own.EchoOf(m.Body.ICAO24, m.Body.Callsign, registration, m.Body.Position.LatLon()); ok {
			counters.Inc(CounterEchoOwnFlight)
			return fid, nil
		}
	}
	c5, _, err := cell.Key(m.Body.Position.LatLon())
	if err != nil {
		counters.Inc(CounterPublishFailed)
		return "", err
	}
	subject, err := bus.Man(c5, m.Body.ICAO24)
	if err != nil {
		counters.Inc(CounterPublishFailed)
		return "", err
	}
	if err := sink.Publish(ctx, subject, m); err != nil {
		counters.Inc(CounterPublishFailed)
		return "", err
	}
	counters.Inc(CounterPublished)
	return "", nil
}

// NormRegistration is a registration or callsign as the echo guard
// compares them: upper case, without spaces, dashes and dots ("4L-ABC"
// and "4LABC" are one registration).
func NormRegistration(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if r == ' ' || r == '-' || r == '.' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
