package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Access is what one operation requires of its caller. Exactly one of
// three shapes: Public (no credential); or one or more of Scopes (a
// machine token granting any one of them), AllScopes (a machine token
// granting every one of them: an OpenAPI security requirement that
// lists several scopes) and Sessions (a session token of one of these
// realms, with one of the listed roles). An Access that is none of them
// is not "anyone": NewGuardedMux refuses it.
type Access struct {
	Public    bool
	Scopes    []string
	AllScopes []string
	Sessions  []SessionAccess
}

// SessionAccess admits a session of Realm whose roles contain one of
// Roles; an empty Roles admits every role of the realm.
type SessionAccess struct {
	Realm string
	Roles []string
}

// Validate refuses an Access that grants nothing or contradicts itself.
func (a Access) Validate() error {
	switch {
	case a.Public && (len(a.Scopes) > 0 || len(a.AllScopes) > 0 || len(a.Sessions) > 0):
		return errors.New("public and restricted at once")
	case !a.Public && len(a.Scopes) == 0 && len(a.AllScopes) == 0 && len(a.Sessions) == 0:
		return errors.New("neither public nor restricted to a scope or a session realm")
	case slices.Contains(a.Scopes, "") || slices.Contains(a.AllScopes, ""):
		return errors.New("an empty scope")
	}
	for _, s := range a.Sessions {
		if s.Realm == "" {
			return errors.New("a session entry without a realm")
		}
	}
	return nil
}

// String renders a for logs and errors.
func (a Access) String() string {
	if a.Public {
		return "public"
	}
	var parts []string
	for _, s := range a.Scopes {
		parts = append(parts, "scope:"+s)
	}
	if len(a.AllScopes) > 0 {
		parts = append(parts, "scopes:"+strings.Join(a.AllScopes, "+"))
	}
	for _, s := range a.Sessions {
		if len(s.Roles) == 0 {
			parts = append(parts, "session:"+s.Realm)
			continue
		}
		parts = append(parts, "session:"+s.Realm+"/"+strings.Join(s.Roles, "|"))
	}
	return strings.Join(parts, " or ")
}

// Guard returns the middleware that enforces one Access.
type Guard func(Access) func(http.Handler) http.Handler

// GuardedMux registers operations on a ServeMux only through their
// entry in an access table, so every route fails closed: a pattern
// without an entry is not registered and is an error, an entry that is
// invalid (Access.Validate, then validate) is an error, and an entry
// that no pattern used is an error (a renamed path must not leave its
// protection behind). The process refuses to start on Err. It
// implements the ServeMux interface of the generated router.
type GuardedMux struct {
	mux      *http.ServeMux
	table    map[string]Access
	guard    Guard
	validate func(Access) error
	used     map[string]bool
	errs     []error
}

// NewGuardedMux wraps mux. table maps a ServeMux pattern ("POST
// /oauth/token") to its Access; guard enforces an Access; validate
// (may be nil) refuses an Access the caller's catalogue does not know.
func NewGuardedMux(mux *http.ServeMux, table map[string]Access, guard Guard, validate func(Access) error) *GuardedMux {
	return &GuardedMux{mux: mux, table: table, guard: guard, validate: validate, used: map[string]bool{}}
}

// HandleFunc registers h under pattern behind its table entry, or
// records why it cannot.
func (g *GuardedMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	a, ok := g.table[pattern]
	if !ok {
		g.errs = append(g.errs, fmt.Errorf("%s has no access entry; the route is not served", pattern))
		return
	}
	g.used[pattern] = true
	if err := a.Validate(); err != nil {
		g.errs = append(g.errs, fmt.Errorf("%s: access %w", pattern, err))
		return
	}
	if g.validate != nil {
		if err := g.validate(a); err != nil {
			g.errs = append(g.errs, fmt.Errorf("%s: %w", pattern, err))
			return
		}
	}
	var handler http.Handler = http.HandlerFunc(h)
	if !a.Public {
		handler = g.guard(a)(handler)
	}
	g.mux.Handle(pattern, handler)
}

// ServeHTTP serves the wrapped mux.
func (g *GuardedMux) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.mux.ServeHTTP(w, r) }

// Err is every problem found so far, including the table entries no
// pattern used; nil when every route has a valid entry and every entry
// a route. Call it after the last registration.
func (g *GuardedMux) Err() error {
	errs := slices.Clone(g.errs)
	var unused []string
	for p := range g.table {
		if !g.used[p] {
			unused = append(unused, p)
		}
	}
	slices.Sort(unused)
	for _, p := range unused {
		errs = append(errs, fmt.Errorf("access entry %s matches no route", p))
	}
	return errors.Join(errs...)
}
