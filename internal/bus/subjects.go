package bus

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/cell"
)

// Subject kinds: the first token(s) of every subject of docs/PLAN.md §7.
const (
	KindTrk     = "trk"
	KindMan     = "man"
	KindAlrt    = "alrt"
	KindConf    = "conf"
	KindIdent   = "ident"
	KindIntent  = "intent"
	KindCIS     = "cis"
	KindPeer    = "peer"
	KindTraffic = "traffic.product"
	KindIngest  = "ingest"
	KindSrc     = "src"
	KindCtl     = "ctl"
)

// The control subjects (core push beside their KV bucket).
const (
	CtlSources  = "ctl.sources"
	CtlPolicy   = "ctl.policy"
	CtlDSSState = "ctl.dss_state"
)

// Stream filters: every subject of one kind.
const (
	SubjectTrkAll     = "trk.v1.>"
	SubjectManAll     = "man.v1.>"
	SubjectAlrtAll    = "alrt.v1.>"
	SubjectConfAll    = "conf.v1.>"
	SubjectIdentAll   = "ident.v1.>"
	SubjectIntentAll  = "intent.v1.>"
	SubjectCISAll     = "cis.v1.>"
	SubjectPeerAll    = "peer.v1.>"
	SubjectTrafficAll = "traffic.product.v1.>"
	SubjectIngestAll  = "ingest.v1.>"
	SubjectSrcAll     = "src.v1.>"
)

// MaxTokenBytes bounds one variable token of a subject (an id, a kind, a
// state, a dataset): longer is refused, never truncated.
const MaxTokenBytes = 128

// MaxSubjectBytes bounds a whole subject Parse reads (E-10).
const MaxSubjectBytes = 512

// Subject is a parsed subject: Kind and the tokens its kind has; the
// others are empty.
type Subject struct {
	Kind string
	// Cell3 and Cell5 for trk, man, peer (both) and alrt (Cell5);
	// Cell3 alone for ingest.
	Cell3, Cell5 string
	// ID is the track, icao24, alert, flight, intent, peer flight or
	// client id the subject ends with.
	ID string
	// Sub is the second variable token: the alert kind, the intent
	// state, the CIS dataset, the source type.
	Sub string
	// Instance is the source instance of src.
	Instance string
}

// token checks one variable token: non-empty, at most MaxTokenBytes,
// valid UTF-8 without space, control characters or the subject
// metacharacters . * >.
func token(field, s string) error {
	if s == "" {
		return core.Fieldf(field, "empty")
	}
	if len(s) > MaxTokenBytes {
		return core.Fieldf(field, "longer than %d bytes", MaxTokenBytes)
	}
	if !utf8.ValidString(s) {
		return core.Fieldf(field, "not valid UTF-8")
	}
	for _, r := range s {
		if r == '.' || r == '*' || r == '>' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return core.Fieldf(field, "%q holds a character a subject token cannot", s)
		}
	}
	return nil
}

// located builds <kind>.v1.<cell3>.<cell5>.<id> from a cell5 name: the
// cell3 is the cell5's parent, so the two never disagree.
func located(kind, cell5, idField, id string) (string, error) {
	c3, err := cell.Parent3(cell5)
	if err != nil {
		return "", err
	}
	if err := token(idField, id); err != nil {
		return "", err
	}
	return kind + ".v1." + c3 + "." + cell5 + "." + id, nil
}

// Trk is trk.v1.<cell3>.<cell5>.<track_id>.
func Trk(cell5, trackID string) (string, error) { return located(KindTrk, cell5, "track_id", trackID) }

// Man is man.v1.<cell3>.<cell5>.<icao24>.
func Man(cell5, icao24 string) (string, error) { return located(KindMan, cell5, "icao24", icao24) }

// Peer is peer.v1.<cell3>.<cell5>.<rid_flight_id>.
func Peer(cell5, ridFlightID string) (string, error) {
	return located(KindPeer, cell5, "rid_flight_id", ridFlightID)
}

// Alrt is alrt.v1.<kind>.<cell5>.<alert_id>.
func Alrt(alertKind, cell5, alertID string) (string, error) {
	if err := token("kind", alertKind); err != nil {
		return "", err
	}
	if _, err := cell.Parse5(cell5); err != nil {
		return "", err
	}
	if err := token("alert_id", alertID); err != nil {
		return "", err
	}
	return "alrt.v1." + alertKind + "." + cell5 + "." + alertID, nil
}

func one(prefix, field, id string) (string, error) {
	if err := token(field, id); err != nil {
		return "", err
	}
	return prefix + id, nil
}

func two(prefix, f1, t1, f2, t2 string) (string, error) {
	if err := token(f1, t1); err != nil {
		return "", err
	}
	if err := token(f2, t2); err != nil {
		return "", err
	}
	return prefix + t1 + "." + t2, nil
}

// Conf is conf.v1.<flight_id>.
func Conf(flightID string) (string, error) { return one("conf.v1.", "flight_id", flightID) }

// Ident is ident.v1.<track_id>.
func Ident(trackID string) (string, error) { return one("ident.v1.", "track_id", trackID) }

// Intent is intent.v1.<state>.<intent_id>.
func Intent(state, intentID string) (string, error) {
	return two("intent.v1.", "state", state, "intent_id", intentID)
}

// CIS is cis.v1.<dataset>.
func CIS(dataset string) (string, error) { return one("cis.v1.", "dataset", dataset) }

// TrafficProduct is traffic.product.v1.<client_id>.
func TrafficProduct(clientID string) (string, error) {
	return one("traffic.product.v1.", "client_id", clientID)
}

// Ingest is ingest.v1.<cell3>.
func Ingest(cell3 string) (string, error) {
	if _, err := cell.Parse3(cell3); err != nil {
		return "", err
	}
	return "ingest.v1." + cell3, nil
}

// Src is src.v1.<type>.<instance>.
func Src(sourceType, instance string) (string, error) {
	return two("src.v1.", "source_type", sourceType, "instance", instance)
}

// TrkCell3 is the filter of every trk subject of one cell3 (a monitor's
// owned cell).
func TrkCell3(cell3 string) (string, error) {
	if _, err := cell.Parse3(cell3); err != nil {
		return "", err
	}
	return "trk.v1." + cell3 + ".>", nil
}

// Parse reads a subject of docs/PLAN.md §7. A subject of an unknown
// kind, a wrong token count, an empty token, a cell that is not one or
// a cell5 outside its cell3 is a *core.FieldError naming "subject";
// nothing here panics on any input.
func Parse(s string) (Subject, error) {
	bad := func(format string, a ...any) (Subject, error) {
		return Subject{}, core.Fieldf("subject", format, a...)
	}
	if len(s) > MaxSubjectBytes {
		return bad("longer than %d bytes", MaxSubjectBytes)
	}
	t := strings.Split(s, ".")
	if len(t) < 2 {
		return bad("%q has too few tokens", s)
	}
	switch {
	case s == CtlSources || s == CtlPolicy || s == CtlDSSState:
		return Subject{Kind: KindCtl, Sub: t[1]}, nil
	case len(t) >= 3 && t[0] == "traffic" && t[1] == "product" && t[2] == "v1":
		if len(t) != 4 {
			return bad("traffic.product.v1 takes 1 token after the version, %q has %d", s, len(t)-3)
		}
		if err := token("client_id", t[3]); err != nil {
			return bad("%v", err)
		}
		return Subject{Kind: KindTraffic, ID: t[3]}, nil
	}
	if t[1] != "v1" {
		return bad("%q is not a v1 subject", s)
	}
	want := map[string]int{
		KindTrk: 5, KindMan: 5, KindPeer: 5, KindAlrt: 5, KindConf: 3, KindIdent: 3,
		KindIntent: 4, KindCIS: 3, KindIngest: 3, KindSrc: 4,
	}
	n, ok := want[t[0]]
	if !ok {
		return bad("%q: unknown kind %q", s, t[0])
	}
	if len(t) != n {
		return bad("%s takes %d tokens, %q has %d", t[0], n, s, len(t))
	}
	for i, tok := range t[2:] {
		if err := token("token", tok); err != nil {
			return bad("token %d of %q: %v", i+3, s, err)
		}
	}
	out := Subject{Kind: t[0]}
	switch t[0] {
	case KindTrk, KindMan, KindPeer:
		p, err := cell.Parent3(t[3])
		if err != nil {
			return bad("%q: %v", s, err)
		}
		if p != t[2] {
			return bad("%q: %s is not in %s", s, t[3], t[2])
		}
		out.Cell3, out.Cell5, out.ID = t[2], t[3], t[4]
	case KindAlrt:
		if _, err := cell.Parse5(t[3]); err != nil {
			return bad("%q: %v", s, err)
		}
		out.Sub, out.Cell5, out.ID = t[2], t[3], t[4]
	case KindConf, KindIdent, KindCIS:
		if t[0] == KindCIS {
			out.Sub = t[2]
		} else {
			out.ID = t[2]
		}
	case KindIntent:
		out.Sub, out.ID = t[2], t[3]
	case KindIngest:
		if _, err := cell.Parse3(t[2]); err != nil {
			return bad("%q: %v", s, err)
		}
		out.Cell3 = t[2]
	case KindSrc:
		out.Sub, out.Instance = t[2], t[3]
	}
	return out, nil
}

// coreKinds travel on core NATS (best effort, no acknowledgement): the
// hot path never blocks on a stream (TRK, MAN and PEER capture them).
var coreKinds = map[string]bool{KindTrk: true, KindMan: true, KindPeer: true, KindSrc: true, KindCtl: true}

// Durable reports whether a subject of kind is published to JetStream
// with an acknowledgement and a dedupe id.
func Durable(kind string) bool { return !coreKinds[kind] }
