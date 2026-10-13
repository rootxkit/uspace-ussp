package testif

import (
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/config"
)

// Process is the name of the test-interfaces process: its log name, its
// metric prefix and its readiness owner. It is not one of the seven
// processes of internal/config (D13): no production deployment runs it.
const Process = "test-interfaces"

// The switch and the system id rule (D13; WP-19 for the hook, enforced
// here by the binary).
const (
	VarSwitch   = "USSP_TEST_INTERFACES"
	VarSystemID = "USSP_SYSTEM_ID"
	SwitchOn    = "on"
)

// systemIDMarkers are the tokens one of which a test or development
// system id contains (systemIDTokens).
var systemIDMarkers = []string{"TEST", "DEV"}

// systemIDTokens splits a system id at its token boundaries: a token is
// a maximal run of letters, so a digit or any other character ends one.
// DEV01, TEST, USSP-DEV and TEST-02 carry a marker; LATEST, DEVON and
// LABTEST1 do not, since their TEST or DEV is part of a longer word.
func systemIDTokens(id string) []string {
	return strings.FieldsFunc(id, func(r rune) bool { return !unicode.IsLetter(r) })
}

// ErrRefused is the gate's refusal: the process exits 2 on it before it
// opens a listener.
var ErrRefused = errors.New("the InterUSS test interfaces are refused")

// Gate refuses to start unless USSP_TEST_INTERFACES is on and
// USSP_SYSTEM_ID is set and has TEST or DEV as a token (D13), never as
// part of a longer word. It reads the raw
// environment, before any other configuration: an unset system id is
// refused, never taken from a default. The error names both variables.
func Gate(lookup config.LookupFunc) error {
	sw, _ := lookup(VarSwitch)
	id, _ := lookup(VarSystemID)
	sw, id = strings.TrimSpace(sw), strings.TrimSpace(id)
	var why []string
	if sw != SwitchOn {
		why = append(why, VarSwitch+" is "+describe(sw)+", not on")
	}
	if !slices.ContainsFunc(systemIDTokens(id), func(tok string) bool { return slices.Contains(systemIDMarkers, tok) }) {
		why = append(why, VarSystemID+" is "+describe(id)+", which has neither TEST nor DEV as a token")
	}
	if len(why) == 0 {
		return nil
	}
	return errors.Join(ErrRefused, errors.New(strings.Join(why, "; ")+
		" (both are required: "+VarSwitch+"=on and a test or development "+VarSystemID+"; the interfaces are never served in production)"))
}

func describe(v string) string {
	if v == "" {
		return "unset"
	}
	if len(v) > 32 {
		v = v[:32] + "..."
	}
	return strconv.Quote(v)
}

// Config is what test-interfaces reads beside the shared variables of
// internal/config (USSP_SYSTEM_ID, USSP_AUDIENCES, USSP_TOKEN_ISSUERS,
// USSP_LOG_LEVEL, USSP_STATUS_INTERVAL_S, USSP_SHUTDOWN_TIMEOUT_S, the
// HTTP bounds). Every variable is in Variables and in deploy/ENV.md's
// test-interfaces table.
type Config struct {
	Addr string
	// USSPBaseURL is this USSP's published base URL as an operator
	// reaches it: /oauth/token, /v1/intents, /v1/accounts/...,
	// WS /v1/telemetry and /v1/traffic/snapshot are under it.
	USSPBaseURL string
	// CAFile is a PEM bundle the USSP's TLS certificate is checked
	// against (the lab's CA); empty uses the system roots.
	CAFile string
	// OperatorClientID and OperatorSecretFile are the test operator's
	// machine client at this USSP's issuer.
	OperatorClientID   string
	OperatorSecretFile string
	// SessionFile holds a portal session token of the test operator's
	// operator_admin (the serial bindings); ConsoleSessionFile a console
	// session token of a staff support or supervisor user (the
	// observation's bbox snapshot, which only a staff session may ask).
	SessionFile        string
	ConsoleSessionFile string
	// OperatorFile is the test operator fixture
	// (deploy/conformance/test-operator.yaml).
	OperatorFile string
	// Bounds (E-10).
	MaxTests          int
	MaxFlightsPerTest int
	MaxStatesFlight   int
	MaxPlans          int
	CallTimeoutS      int
	AckTimeoutS       int
}

// Variable is one variable of Config, for --help and the ENV.md test.
type Variable struct {
	Name, Default, Unit, Help string
	Required                  bool
	set                       func(*Config, string) error
}

func str(dst func(*Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error { *dst(c) = v; return nil }
}

func urlVar(dst func(*Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("must be an absolute http(s) URL without credentials, query or fragment")
		}
		*dst(c) = strings.TrimSuffix(v, "/")
		return nil
	}
}

func intVar(dst func(*Config) *int, lo, hi int) func(*Config, string) error {
	return func(c *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return errors.New("must be an integer from " + strconv.Itoa(lo) + " to " + strconv.Itoa(hi))
		}
		*dst(c) = n
		return nil
	}
}

// Variables lists every variable test-interfaces reads beside the shared
// ones, in the order of deploy/ENV.md.
func Variables() []Variable {
	return []Variable{
		{Name: VarSwitch, Default: "off", Help: "on starts the InterUSS automated-testing interfaces (with a USSP_SYSTEM_ID that has TEST or DEV as a token: DEV01, not LATEST); anything else exits 2 before a listener opens. Never on in production (D13)",
			set: func(*Config, string) error { return nil }},
		{Name: "USSP_TEST_INTERFACES_ADDR", Default: ":8087", Help: "listen address of test-interfaces (/test/rid/v1/injection, /test/rid/v1/observation, /test/flight_planning/v1, /healthz, /readyz, /metrics)",
			set: str(func(c *Config) *string { return &c.Addr })},
		{Name: "USSP_TEST_INTERFACES_USSP_BASE_URL", Required: true, Help: "this USSP's published base URL as an operator reaches it (token, intents, accounts, WS /v1/telemetry, traffic snapshot)",
			set: urlVar(func(c *Config) *string { return &c.USSPBaseURL })},
		{Name: "USSP_TEST_INTERFACES_CA_FILE", Help: "PEM bundle the USSP's TLS certificate is checked against (the lab's CA); empty uses the system roots",
			set: str(func(c *Config) *string { return &c.CAFile })},
		{Name: "USSP_TEST_INTERFACES_OPERATOR_CLIENT_ID", Required: true, Help: "the test operator's machine client at this USSP's issuer (scopes ussp.telemetry, ussp.intents)",
			set: str(func(c *Config) *string { return &c.OperatorClientID })},
		{Name: "USSP_TEST_INTERFACES_OPERATOR_SECRET_FILE", Required: true, Help: "file holding that client's secret (never the environment or a command line)",
			set: str(func(c *Config) *string { return &c.OperatorSecretFile })},
		{Name: "USSP_TEST_INTERFACES_SESSION_FILE", Help: "file holding a portal session token of the test operator's operator_admin, for the serial bindings; unset, an unbound serial is the injection's failure",
			set: str(func(c *Config) *string { return &c.SessionFile })},
		{Name: "USSP_TEST_INTERFACES_CONSOLE_SESSION_FILE", Help: "file holding a console session token of a staff support or supervisor user, for the observation's bbox snapshot; unset, the observation answers 503",
			set: str(func(c *Config) *string { return &c.ConsoleSessionFile })},
		{Name: "USSP_TEST_INTERFACES_OPERATOR_FILE", Required: true, Help: "the test operator fixture (deploy/conformance/test-operator.yaml): serial pool and the Annex IV items a flight plan lacks (pending GCAA)",
			set: str(func(c *Config) *string { return &c.OperatorFile })},
		{Name: "USSP_TEST_INTERFACES_MAX_TESTS", Default: "16", Help: "RID injection tests held at once; one more is 429",
			set: intVar(func(c *Config) *int { return &c.MaxTests }, 1, 1024)},
		{Name: "USSP_TEST_INTERFACES_MAX_FLIGHTS", Default: "32", Help: "flights of one RID injection test; more is 400",
			set: intVar(func(c *Config) *int { return &c.MaxFlightsPerTest }, 1, 1024)},
		{Name: "USSP_TEST_INTERFACES_MAX_STATES", Default: "7200", Help: "telemetry states of one injected flight; more is 400",
			set: intVar(func(c *Config) *int { return &c.MaxStatesFlight }, 1, 86400)},
		{Name: "USSP_TEST_INTERFACES_MAX_PLANS", Default: "1024", Help: "flight plans and request ids remembered; the oldest is forgotten beyond it (an upsert of a forgotten plan finds its intent again through GET /v1/intents)",
			set: intVar(func(c *Config) *int { return &c.MaxPlans }, 16, 65536)},
		{Name: "USSP_TEST_INTERFACES_CALL_TIMEOUT_S", Default: "10", Unit: "s", Help: "bound on one call to the USSP's API",
			set: intVar(func(c *Config) *int { return &c.CallTimeoutS }, 1, 120)},
		{Name: "USSP_TEST_INTERFACES_ACK_TIMEOUT_S", Default: "15", Unit: "s", Help: "how long an injection waits for telemetry-ingest to acknowledge a flight's first frame, and a delete its end frame",
			set: intVar(func(c *Config) *int { return &c.AckTimeoutS }, 1, 300)},
	}
}

// Load reads Config through lookup. Every problem is a *core.FieldError
// naming the variable; all are returned, joined.
func Load(lookup config.LookupFunc) (Config, error) {
	var c Config
	var errs []error
	for _, v := range Variables() {
		raw, _ := lookup(v.Name)
		raw = strings.TrimSpace(raw)
		if raw == "" {
			raw = v.Default
		}
		if raw == "" {
			if v.Required {
				errs = append(errs, &core.FieldError{Field: v.Name, Reason: "required by " + Process})
			}
			continue
		}
		if err := v.set(&c, raw); err != nil {
			errs = append(errs, &core.FieldError{Field: v.Name, Reason: err.Error()})
		}
	}
	return c, errors.Join(errs...)
}

// Help lists the variables for --help.
func Help() string {
	var b strings.Builder
	for _, v := range Variables() {
		b.WriteString(v.Name)
		if v.Default != "" {
			b.WriteString(" (default " + v.Default + ")")
		}
		if v.Required {
			b.WriteString(" (required)")
		}
		b.WriteString("\n    " + v.Help + "\n")
	}
	return b.String()
}
