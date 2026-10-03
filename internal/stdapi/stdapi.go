// Package stdapi mounts the standard USS interfaces (docs/PLAN.md D3,
// §6.2): the strict servers generated into internal/stdapi/f3411 (on
// rid-sp) and internal/stdapi/f3548 (on api), each operation behind its
// entry in an access table that holds the scopes the standard's OpenAPI
// `security` block names, on an httpx.GuardedMux so that a route without
// an entry is not served. Until WP-9 (F3411) and WP-13 (F3548) replace
// them, NotImplementedF3411 and NotImplementedF3548 answer every
// operation 501 not_implemented, after the guard: a caller without the
// standard's scope still gets 401 or 403, so a stub never teaches a peer
// that an endpoint is open.
//
// The wire conversions live in internal/stdapi/convert.
package stdapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	stdf3411 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3411"
	stdf3548 "github.com/rootxkit/uspace-ussp/internal/stdapi/f3548"
)

// The F3411 and F3548 scopes (the `Authority` security scheme of each
// file).
const (
	ScopeRIDDisplayProvider      = "rid.display_provider"
	ScopeRIDServiceProvider      = "rid.service_provider"
	ScopeStrategicCoordination   = "utm.strategic_coordination"
	ScopeConstraintManagement    = "utm.constraint_management"
	ScopeConstraintProcessing    = "utm.constraint_processing"
	ScopeConformanceMonitoringSA = "utm.conformance_monitoring_sa"
	ScopeAvailabilityArbitration = "utm.availability_arbitration"
)

// SlugNotImplemented is the problem type of a stub's answer.
const SlugNotImplemented = "not_implemented"

// Counter names of the mounted servers (E-09).
const (
	CounterNotImplemented = "stdapi_not_implemented"
	CounterRequestRefused = "stdapi_request_refused"
	CounterHandlerFailed  = "stdapi_handler_failed"
)

// maxParamNameBytes bounds a parameter name echoed in a problem.
const maxParamNameBytes = 64

// ErrNotImplemented is what a stub handler returns; it is answered 501
// not_implemented.
var ErrNotImplemented = errors.New("not implemented until its work package")

func one(scope string) httpx.Access { return httpx.Access{Scopes: []string{scope}} }

// F3411Access is the access entry of every F3411-22a USS-side operation
// rid-sp serves, by ServeMux pattern, from the `security` block of each
// operation in api/standards/f3411-v22a.yaml (one requirement, one
// scope each).
func F3411Access() map[string]httpx.Access {
	return map[string]httpx.Access{
		"GET /uss/flights":                            one(ScopeRIDDisplayProvider), // searchFlights
		"GET /uss/flights/{id}/details":               one(ScopeRIDDisplayProvider), // getFlightDetails
		"POST /uss/identification_service_areas/{id}": one(ScopeRIDServiceProvider), // postIdentificationServiceArea
	}
}

// F3548Access is the access entry of every F3548-21 USS-side operation
// api serves, by ServeMux pattern, from api/standards/f3548-v21.yaml.
// makeUssReport lists five requirements of one scope each (any one);
// getLogSet has no `security` of its own and inherits the file's
// top-level one, a single requirement listing all five scopes (every
// one: AllScopes).
func F3548Access() map[string]httpx.Access {
	all := []string{ScopeStrategicCoordination, ScopeConstraintManagement, ScopeConstraintProcessing,
		ScopeConformanceMonitoringSA, ScopeAvailabilityArbitration}
	return map[string]httpx.Access{
		"GET /uss/v1/operational_intents/{entityid}":           one(ScopeStrategicCoordination),   // getOperationalIntentDetails
		"GET /uss/v1/operational_intents/{entityid}/telemetry": one(ScopeConformanceMonitoringSA), // getOperationalIntentTelemetry
		"POST /uss/v1/operational_intents":                     one(ScopeStrategicCoordination),   // notifyOperationalIntentDetailsChanged
		"GET /uss/v1/constraints/{entityid}":                   one(ScopeConstraintProcessing),    // getConstraintDetails
		"POST /uss/v1/constraints":                             one(ScopeConstraintManagement),    // notifyConstraintDetailsChanged
		"POST /uss/v1/reports": {Scopes: []string{ScopeStrategicCoordination, ScopeConstraintProcessing, // makeUssReport
			ScopeConstraintManagement, ScopeConformanceMonitoringSA, ScopeAvailabilityArbitration}},
		"GET /uss/v1/log_sets/{log_set_id}": {AllScopes: all}, // getLogSet
	}
}

// Options are what a mount needs beside the handlers.
type Options struct {
	// Guard enforces an access entry (auth.Guard.Require).
	Guard httpx.Guard
	// Validate refuses an access entry outside the scope catalogue
	// (auth.ValidateAccess); nil skips it.
	Validate func(httpx.Access) error
	// Counters receives CounterNotImplemented, CounterRequestRefused and
	// CounterHandlerFailed; nil counts nothing. A handler that fails
	// logs its own cause; the answer never carries it.
	Counters *core.Counters
}

// MountF3411 registers the F3411 USS server s on mux behind
// F3411Access. The error lists every route without a valid entry and
// every entry without a route; the process refuses to start on it.
func MountF3411(mux *http.ServeMux, s stdf3411.StrictServerInterface, o Options) error {
	if o.Guard == nil {
		return core.Fieldf("guard", "nil: every route needs one")
	}
	g := httpx.NewGuardedMux(mux, F3411Access(), o.Guard, o.Validate)
	h := stdf3411.NewStrictHandlerWithOptions(s, nil, stdf3411.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: o.requestError, ResponseErrorHandlerFunc: o.responseError,
	})
	stdf3411.HandlerWithOptions(h, stdf3411.StdHTTPServerOptions{BaseRouter: g, ErrorHandlerFunc: o.paramError(paramName3411)})
	return g.Err()
}

// MountF3548 registers the F3548 USS server s on mux behind
// F3548Access, like MountF3411.
func MountF3548(mux *http.ServeMux, s stdf3548.StrictServerInterface, o Options) error {
	if o.Guard == nil {
		return core.Fieldf("guard", "nil: every route needs one")
	}
	g := httpx.NewGuardedMux(mux, F3548Access(), o.Guard, o.Validate)
	h := stdf3548.NewStrictHandlerWithOptions(s, nil, stdf3548.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: o.requestError, ResponseErrorHandlerFunc: o.responseError,
	})
	stdf3548.HandlerWithOptions(h, stdf3548.StdHTTPServerOptions{BaseRouter: g, ErrorHandlerFunc: o.paramError(paramName3548)})
	return g.Err()
}

func (o Options) count(name string) {
	if o.Counters != nil {
		o.Counters.Inc(name)
	}
}

// requestError answers a body the strict server could not decode; one
// cut at the body cap is 413, not a malformed body (audit N2).
func (o Options) requestError(w http.ResponseWriter, r *http.Request, err error) {
	o.count(CounterRequestRefused)
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.NewProblem(http.StatusBadRequest, httpx.SlugValidation, "", "the request body is not the operation's JSON",
		core.Fieldf("body", "not the expected JSON object")).Write(w, r)
}

// responseError answers a handler error: ErrNotImplemented is 501, any
// other error 500 whose cause is never sent.
func (o Options) responseError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotImplemented) {
		o.count(CounterNotImplemented)
		httpx.NewProblem(http.StatusNotImplemented, SlugNotImplemented, "", "this operation is not implemented yet").Write(w, r)
		return
	}
	o.count(CounterHandlerFailed)
	httpx.NewProblem(http.StatusInternalServerError, httpx.SlugInternal, "", "").Write(w, r)
}

// paramError answers a path or query parameter the generated router
// could not bind, naming the parameter when the error says which.
func (o Options) paramError(name func(error) string) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		o.count(CounterRequestRefused)
		field := name(err)
		if field == "" || len(field) > maxParamNameBytes {
			field = "parameter"
		}
		httpx.NewProblem(http.StatusBadRequest, httpx.SlugValidation, "", "a parameter is malformed or missing",
			core.Fieldf(field, "malformed or missing")).Write(w, r)
	}
}

func paramName3411(err error) string {
	var (
		inv *stdf3411.InvalidParamFormatError
		req *stdf3411.RequiredParamError
	)
	switch {
	case errors.As(err, &inv):
		return inv.ParamName
	case errors.As(err, &req):
		return req.ParamName
	}
	return ""
}

func paramName3548(err error) string {
	var (
		inv *stdf3548.InvalidParamFormatError
		req *stdf3548.RequiredParamError
	)
	switch {
	case errors.As(err, &inv):
		return inv.ParamName
	case errors.As(err, &req):
		return req.ParamName
	}
	return ""
}

// NotImplementedF3411 answers every F3411 USS operation 501 until WP-9.
type NotImplementedF3411 struct{}

var _ stdf3411.StrictServerInterface = NotImplementedF3411{}

// SearchFlights is not implemented (WP-9).
func (NotImplementedF3411) SearchFlights(context.Context, stdf3411.SearchFlightsRequestObject) (stdf3411.SearchFlightsResponseObject, error) {
	return nil, ErrNotImplemented
}

// GetFlightDetails is not implemented (WP-9).
func (NotImplementedF3411) GetFlightDetails(context.Context, stdf3411.GetFlightDetailsRequestObject) (stdf3411.GetFlightDetailsResponseObject, error) {
	return nil, ErrNotImplemented
}

// PostIdentificationServiceArea is not implemented (WP-9).
func (NotImplementedF3411) PostIdentificationServiceArea(context.Context, stdf3411.PostIdentificationServiceAreaRequestObject) (stdf3411.PostIdentificationServiceAreaResponseObject, error) {
	return nil, ErrNotImplemented
}

// NotImplementedF3548 answers every F3548 USS operation 501 until WP-13.
type NotImplementedF3548 struct{}

var _ stdf3548.StrictServerInterface = NotImplementedF3548{}

// GetOperationalIntentDetails is not implemented (WP-13).
func (NotImplementedF3548) GetOperationalIntentDetails(context.Context, stdf3548.GetOperationalIntentDetailsRequestObject) (stdf3548.GetOperationalIntentDetailsResponseObject, error) {
	return nil, ErrNotImplemented
}

// GetOperationalIntentTelemetry is not implemented (WP-13).
func (NotImplementedF3548) GetOperationalIntentTelemetry(context.Context, stdf3548.GetOperationalIntentTelemetryRequestObject) (stdf3548.GetOperationalIntentTelemetryResponseObject, error) {
	return nil, ErrNotImplemented
}

// NotifyOperationalIntentDetailsChanged is not implemented (WP-13).
func (NotImplementedF3548) NotifyOperationalIntentDetailsChanged(context.Context, stdf3548.NotifyOperationalIntentDetailsChangedRequestObject) (stdf3548.NotifyOperationalIntentDetailsChangedResponseObject, error) {
	return nil, ErrNotImplemented
}

// GetConstraintDetails is not implemented (WP-13).
func (NotImplementedF3548) GetConstraintDetails(context.Context, stdf3548.GetConstraintDetailsRequestObject) (stdf3548.GetConstraintDetailsResponseObject, error) {
	return nil, ErrNotImplemented
}

// NotifyConstraintDetailsChanged is not implemented (WP-13).
func (NotImplementedF3548) NotifyConstraintDetailsChanged(context.Context, stdf3548.NotifyConstraintDetailsChangedRequestObject) (stdf3548.NotifyConstraintDetailsChangedResponseObject, error) {
	return nil, ErrNotImplemented
}

// MakeUssReport is not implemented (WP-13).
func (NotImplementedF3548) MakeUssReport(context.Context, stdf3548.MakeUssReportRequestObject) (stdf3548.MakeUssReportResponseObject, error) {
	return nil, ErrNotImplemented
}

// GetLogSet is not implemented (WP-13).
func (NotImplementedF3548) GetLogSet(context.Context, stdf3548.GetLogSetRequestObject) (stdf3548.GetLogSetResponseObject, error) {
	return nil, ErrNotImplemented
}
