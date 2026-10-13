package testif

import (
	"fmt"
	"slices"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
)

// The scopes of the InterUSS automated-testing interfaces, as the pinned
// files' `security` blocks name them (api/standards/interuss/; Q17: the
// file wins; TestScopesAreThePinnedFiles reads them). They are the test
// listener's closed list and NOT catalogue scopes: internal/auth's
// EcosystemScopes does not hold them, so a production route that named
// one would fail auth.ValidateAccess at start (D13).
const (
	// ScopeInjectTestData is rid/v1/injection.yaml's TestAuth scope.
	ScopeInjectTestData = "rid.inject_test_data"
	// ScopeObserve is rid/v1/observation.yaml's RIDAuth scope.
	ScopeObserve = "dss.read.identification_service_areas"
	// ScopeDirectAutomatedTest and ScopePlan are
	// flight_planning/v1/flight_planning.yaml's Authority scopes.
	ScopeDirectAutomatedTest = "interuss.flight_planning.direct_automated_test"
	ScopePlan                = "interuss.flight_planning.plan"
)

// Scopes is the closed list, in the order above.
var Scopes = []string{ScopeInjectTestData, ScopeObserve, ScopeDirectAutomatedTest, ScopePlan}

// The base paths of the three interfaces on the test listener (§6.4).
const (
	BaseInjection      = "/test/rid/v1/injection"
	BaseObservation    = "/test/rid/v1/observation"
	BaseFlightPlanning = "/test/flight_planning/v1"
)

func one(scope string) httpx.Access { return httpx.Access{Scopes: []string{scope}} }

// InjectionAccess is the access entry of each RID injection operation,
// by ServeMux pattern under BaseInjection.
func InjectionAccess() map[string]httpx.Access {
	return map[string]httpx.Access{
		"PUT " + BaseInjection + "/tests/{test_id}":              one(ScopeInjectTestData), // createTest
		"DELETE " + BaseInjection + "/tests/{test_id}/{version}": one(ScopeInjectTestData), // deleteTest
		"GET " + BaseInjection + "/user_notifications":           one(ScopeInjectTestData), // QueryUserNotifications
	}
}

// ObservationAccess is the access entry of each RID observation
// operation under BaseObservation.
func ObservationAccess() map[string]httpx.Access {
	return map[string]httpx.Access{
		"GET " + BaseObservation + "/display_data":      one(ScopeObserve), // getDisplayData
		"GET " + BaseObservation + "/display_data/{id}": one(ScopeObserve), // getDetails
	}
}

// FlightPlanningAccess is the access entry of each flight_planning v1
// operation under BaseFlightPlanning, each the scope its operation names.
func FlightPlanningAccess() map[string]httpx.Access {
	return map[string]httpx.Access{
		"GET " + BaseFlightPlanning + "/status":                           one(ScopeDirectAutomatedTest), // GetStatus
		"POST " + BaseFlightPlanning + "/clear_area_requests":             one(ScopeDirectAutomatedTest), // ClearArea
		"PUT " + BaseFlightPlanning + "/flight_plans/{flight_plan_id}":    one(ScopePlan),                // UpsertFlightPlan
		"DELETE " + BaseFlightPlanning + "/flight_plans/{flight_plan_id}": one(ScopeDirectAutomatedTest), // DeleteFlightPlan
		"GET " + BaseFlightPlanning + "/user_notifications":               one(ScopePlan),                // QueryUserNotifications
	}
}

// ValidateAccess refuses an access entry of the test listener that is
// not one of the closed list's scopes: no session, no public route, no
// catalogue scope.
func ValidateAccess(a httpx.Access) error {
	if a.Public || len(a.Sessions) > 0 || len(a.AllScopes) > 0 || a.WebSocket || len(a.Scopes) == 0 {
		return fmt.Errorf("the test listener admits one of its own scopes only, not %s", a)
	}
	for _, s := range a.Scopes {
		if !slices.Contains(Scopes, s) {
			return fmt.Errorf("scope %q is not an InterUSS automated-testing scope of the pinned files", s)
		}
	}
	return nil
}
