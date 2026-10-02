package ridsp

import (
	"context"
	"math"

	"github.com/rootxkit/uspace-core/f3411"
	"github.com/rootxkit/uspace-core/regnum"

	stdf3411 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3411"
	"github.com/rootxkit/uspace-ussp/internal/telemetry"
)

// GetFlightDetails is GET /uss/flights/{id}/details: the details of a
// flight in the window, 404 with the standard's ErrorResponse for any
// other id. Served only behind rid.display_provider (the access table):
// operator_location is the remote pilot's personal data (spec 06 §5),
// and it is never logged.
func (s *Server) GetFlightDetails(_ context.Context, req stdf3411.GetFlightDetailsRequestObject) (stdf3411.GetFlightDetailsResponseObject, error) {
	s.count(CounterDetailsRequests)
	v, ok := s.Window.Flight(req.Id)
	if !ok {
		s.count(CounterDetailsNotFound)
		return stdf3411.GetFlightDetails404JSONResponse{Message: message("no flight of this Service Provider with that id in the last %d s",
			f3411.NetMaxNearRealTimeDataPeriodSeconds)}, nil
	}
	var in *IntentFacts
	if id := v.Current.Body.IntentID; id != nil && s.Intents != nil {
		if f, ok := s.Intents.Intent(*id); ok {
			in = &f
		}
	}
	return stdf3411.GetFlightDetails200JSONResponse{Details: DetailsOf(v, in)}, nil
}

// categories maps Annex IV item (4) to the F3411 EU category.
var categories = map[string]f3411.UAClassificationEUCategory{
	"open": f3411.Open, "specific": f3411.Specific, "certified": f3411.Certified,
}

// classes maps the class label to the F3411 EU class.
var classes = map[string]f3411.UAClassificationEUClass{
	"C0": f3411.Class0, "C1": f3411.Class1, "C2": f3411.Class2, "C3": f3411.Class3,
	"C4": f3411.Class4, "C5": f3411.Class5, "C6": f3411.Class6,
}

// ClassificationOf is the EU classification of an intent's Annex IV
// declaration; undefined for what it does not declare or a flight
// without an intent.
func ClassificationOf(in *IntentFacts) f3411.UAClassificationEU {
	cat, cls := f3411.EUCategoryUndefined, f3411.EUClassUndefined
	if in != nil {
		if c, ok := categories[in.Category]; ok {
			cat = c
		}
		if in.ClassLabel != nil {
			if c, ok := classes[*in.ClassLabel]; ok {
				cls = c
			}
		}
	}
	return f3411.UAClassificationEU{Category: &cat, Class: &cls}
}

// DetailsOf is the RIDFlightDetails of one flight (spec 04 §3.1, Art.
// 8(2)): uas_id {serial_number, registration_id (the UA registration
// when declared), utm_id (our flight id)}, operator_id (the public part
// of the operator registration number, never its secret part),
// operator_location (the remote pilot's or take-off position of the
// newest sample that carries one, with its WGS84 altitude when sent),
// operation_description (the authorisation number) and
// eu_classification. v.Recent are the flight's earlier samples; in is
// the flight's intent, nil without one.
func DetailsOf(v View, in *IntentFacts) f3411.RIDFlightDetails {
	b := v.Current.Body
	d := f3411.RIDFlightDetails{Id: v.ID}
	uas := f3411.UASID{UtmId: ptr(v.ID)}
	switch {
	case b.Identification.Serial != nil && *b.Identification.Serial != "":
		uas.SerialNumber = ptr(*b.Identification.Serial)
	case in != nil && in.UASSerial != "":
		uas.SerialNumber = ptr(in.UASSerial)
	}
	if in != nil && in.UARegistration != nil && *in.UARegistration != "" {
		uas.RegistrationId = ptr(*in.UARegistration)
	}
	d.UasId = &uas
	if op := operatorOf(b, in); op != "" {
		d.OperatorId = &op
	}
	if in != nil && in.AuthorisationNumber != nil && *in.AuthorisationNumber != "" {
		d.OperationDescription = ptr(*in.AuthorisationNumber)
	}
	if loc := operatorLocationOf(v.Current, v.Recent); loc != nil {
		d.OperatorLocation = loc
	}
	cls := ClassificationOf(in)
	d.EuClassification = &cls
	return d
}

// operatorOf is the public part of the operator's registration number:
// the intent's, else the one the identification resolved, else the one
// it carries.
func operatorOf(b telemetry.TrackBody, in *IntentFacts) string {
	var raw string
	switch {
	case in != nil && in.OperatorReg != "":
		raw = in.OperatorReg
	case b.Identification.RegisteredOperatorReg != nil:
		raw = *b.Identification.RegisteredOperatorReg
	case b.Identification.OperatorReg != nil:
		raw = *b.Identification.OperatorReg
	}
	if raw == "" {
		return ""
	}
	return regnum.PublicPart(raw)
}

// operatorLocationOf is the operator position of cur, or of the newest
// earlier sample that has one; nil when none has. The altitude type is
// not sent: telemetry/v1 does not say whether the position is the
// remote pilot's live one or the take-off point (spec gap).
func operatorLocationOf(cur Sample, earlier []Sample) *f3411.OperatorLocation {
	op := cur.Body.OperatorPosition
	for i := len(earlier) - 1; op == nil && i >= 0; i-- {
		op = earlier[i].Body.OperatorPosition
	}
	if op == nil {
		return nil
	}
	loc := &f3411.OperatorLocation{Position: f3411.LatLngPoint{Lat: op.Lat, Lng: op.Lng}}
	if op.AltWGS84M != nil && !math.IsNaN(*op.AltWGS84M) && !math.IsInf(*op.AltWGS84M, 0) {
		loc.Altitude = &f3411.Altitude{Reference: f3411.W84, Units: f3411.AltitudeUnitsM, Value: *op.AltWGS84M}
	}
	return loc
}
