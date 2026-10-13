package testif

import (
	"encoding/json"
	"math"
	"slices"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/national/client"
	fp "github.com/rootxkit/uspace-ussp/internal/stdapi/interuss/flightplanning"
)

// The enumerations of intent/request/v1 (schemas/intent/request/v1, the
// source of truth; TestAnnexIVMappingIsTheSchemas holds every value
// below to the schema), from the FlightAuthorisationData values of the
// pinned file. A value the tables do not map refuses the plan, naming
// it; nothing is guessed (pending GCAA where the reading is the lab's).
var (
	categories = map[fp.FlightAuthorisationDataOperationCategory]string{
		fp.FlightAuthorisationDataOperationCategoryOpen:      "open",
		fp.FlightAuthorisationDataOperationCategorySpecific:  "specific",
		fp.FlightAuthorisationDataOperationCategoryCertified: "certified",
	}
	modes = map[fp.OperationMode]string{
		fp.OperationModeVlos:  "VLOS",
		fp.OperationModeBvlos: "BVLOS",
	}
	classes = map[fp.UASClass]string{
		fp.UASClassC0: "C0", fp.UASClassC1: "C1", fp.UASClassC2: "C2", fp.UASClassC3: "C3",
		fp.UASClassC4: "C4", fp.UASClassC5: "C5", fp.UASClassC6: "C6",
	}
)

const (
	flightTypeNormal  = "normal"
	flightTypeSpecial = "special_operation"
	idTechBoth        = "both"
	timeFormatRFC3339 = "RFC3339"
)

// request maps a FlightPlan onto intent/request/v1 (brief WP-21,
// flight_planning v1): the U-space flight authorisation's members onto
// the ten Annex IV items, the F3548 priority onto the flight type, the
// area onto the volumes (as the plan's JSON), and the items the file has
// no member for (the open subcategory, contingency, emergency contact
// reference, pilot, take-off, landing) from the test operator's
// fixture. A plan without uspace_flight_authorisation, with a value the
// tables do not map, or in the open category while the fixture declares
// no subcategory, is refused before any call.
func (p *planner) request(plan fp.FlightPlan, clientRef string, volumes json.RawMessage) (intentBody, error) {
	fa := plan.UspaceFlightAuthorisation
	if fa == nil {
		return intentBody{}, refusedItem("uspace_flight_authorisation",
			"the plan carries no U-space flight authorisation data: Annex IV items 1, 2, 4 and 6 to 10 are the operator's to give")
	}
	cat, ok := categories[fa.OperationCategory]
	if !ok {
		return intentBody{}, refusedItem("uspace_flight_authorisation.operation_category", "%q has no Annex IV category", string(fa.OperationCategory))
	}
	mode, ok := modes[fa.OperationMode]
	if !ok {
		return intentBody{}, refusedItem("uspace_flight_authorisation.operation_mode", "%q has no Annex IV mode", string(fa.OperationMode))
	}
	tech, err := p.identification(fa.IdentificationTechnologies)
	if err != nil {
		return intentBody{}, err
	}
	if fa.EnduranceMinutes <= 0 || math.IsInf(float64(fa.EnduranceMinutes), 0) || math.IsNaN(float64(fa.EnduranceMinutes)) {
		return intentBody{}, refusedItem("uspace_flight_authorisation.endurance_minutes", "not a positive number of minutes")
	}
	fx := p.s.fixture.AnnexIV
	var sub any
	if cat == "open" {
		// Item 4 in the open category is the subcategory too; the plan
		// has no member for it: it follows the plan's class by the test
		// operator's table, else the test operator's one subcategory.
		if s := fx.OpenSubcategoryByClass[classes[fa.UasClass]]; s != "" {
			sub = s
		} else if fx.OpenSubcategory != "" {
			sub = fx.OpenSubcategory
		} else {
			return intentBody{}, refusedItem("uspace_flight_authorisation.operation_category",
				"Open: the plan names no subcategory (Annex IV item 4) and the test operator declares none for class %q (annex_iv.open_subcategory_by_class, annex_iv.open_subcategory, pending GCAA)", string(fa.UasClass))
		}
	}
	r := client.IntentRequest{
		ClientRef:                clientRef,
		UasSerial:                fa.UasSerialNumber,
		Mode:                     mode,
		Category:                 cat,
		Subcategory:              sub,
		IdentificationTechnology: tech,
		ConnectivityMethods:      slices.Clone(fa.ConnectivityMethods),
		EnduranceS:               int(math.Round(float64(fa.EnduranceMinutes) * 60)),
		LossOfC2Procedure:        fa.EmergencyProcedureUrl,
		OperatorReg:              fa.OperatorId,
		EmergencyContactRef:      fx.EmergencyContactRef,
		FlightType:               flightTypeNormal,
	}
	r.Contingency.Procedure = fx.Contingency.Procedure
	if len(fx.Contingency.LandingSites) > 0 {
		sites := make([]client.IntentPoint, 0, len(fx.Contingency.LandingSites))
		for _, s := range fx.Contingency.LandingSites {
			sites = append(sites, point(s))
		}
		r.Contingency.LandingSites = &sites
	}
	if c, ok := classes[fa.UasClass]; ok {
		r.ClassLabel = c
	}
	if fa.UasTypeCertificate != nil && *fa.UasTypeCertificate != "" {
		tc := *fa.UasTypeCertificate
		r.TypeCertificate = &tc
	}
	if fa.UasId != nil && *fa.UasId != "" {
		reg := *fa.UasId
		r.UaRegistration = &reg
	}
	if fx.PilotRef != "" {
		pr := fx.PilotRef
		r.PilotRef = &pr
	}
	if fx.Takeoff != nil {
		t := point(*fx.Takeoff)
		r.Takeoff = &t
	}
	if fx.Landing != nil {
		l := point(*fx.Landing)
		r.Landing = &l
	}
	if plan.AstmF354821 != nil && plan.AstmF354821.Priority != nil && *plan.AstmF354821.Priority != 0 {
		// Above the normal priority is a special operation (SERA Art. 4);
		// the USSP refuses a priority that is not its policy's for one.
		pr := *plan.AstmF354821.Priority
		r.FlightType, r.Priority = flightTypeSpecial, &pr
	}
	return intentBody{IntentRequest: r, Volumes: volumes}, nil
}

// point is a fixture point as the request's. The generated client
// holds IntentPoint as float32: a fixture point is a declared take-off,
// landing or landing site, not a volume vertex.
func point(p LatLngY) client.IntentPoint {
	return client.IntentPoint{Lat: float32(p.Lat), Lng: float32(p.Lng)}
}

// identification maps the plan's identification technologies onto the
// request's one value through the fixture's table: network and direct
// together are both.
func (p *planner) identification(techs []string) (string, error) {
	if len(techs) == 0 {
		return "", refusedItem("uspace_flight_authorisation.identification_technologies", "names none")
	}
	seen := map[string]bool{}
	for _, t := range techs {
		v, ok := p.s.fixture.IdentificationTechnologies[t]
		if !ok {
			return "", refusedItem("uspace_flight_authorisation.identification_technologies", "%q is not in the test operator's table", t)
		}
		seen[v] = true
	}
	if seen[idTechBoth] || (seen["network"] && seen["direct"]) {
		return idTechBoth, nil
	}
	for v := range seen {
		return v, nil
	}
	return "", refusedItem("uspace_flight_authorisation.identification_technologies", "names none")
}

// refusedItem is a plan the USSP's request cannot carry: Rejected, as a
// validation refusal of the USSP would be.
func refusedItem(field, format string, a ...any) error {
	fe := core.Fieldf(field, format, a...)
	return &RefusalError{Status: 400, Reason: "annex_iv_invalid", Detail: fe.Field + ": " + fe.Reason}
}
