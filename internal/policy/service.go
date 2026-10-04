package policy

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/rootxkit/uspace-core/core"
)

// Counter names of the service.
const (
	CounterProjectionFailed = "policy_projection_failed" // a Put refused because the KV projection failed
	CounterPut              = "policy_put"               // a new version committed
)

// BucketPolicy is the KV bucket the policy is projected to (D6).
const BucketPolicy = "policy"

// ErrNoPolicy is returned by Load when no version is stored yet.
var ErrNoPolicy = errors.New("no policy version stored")

// AnyBase is the base of a Put that replaces whatever version is in
// force (a bootstrap, a test); a change a person made on a version they
// read goes through PutOn with that version.
const AnyBase int64 = -1

// StaleBaseError refuses a version made on Base when the newest stored
// version, read under the policy lock, is Current (0 when none is
// stored): another change was stored meanwhile and nothing was written.
type StaleBaseError struct {
	Base, Current int64
}

func (e *StaleBaseError) Error() string {
	return fmt.Sprintf("the policy in force is version %d, not %d", e.Current, e.Base)
}

// Store is the database side (internal/store implements it).
type Store interface {
	// NewestPolicy is the highest version, or ErrNoPolicy.
	NewestPolicy(ctx context.Context) (Record, error)
	// InsertPolicy stores v as a new version with its audit event in one
	// transaction and calls beforeCommit with the stored record inside
	// it; an error from beforeCommit rolls everything back and is
	// returned wrapped. Unless base is AnyBase, the newest version is
	// read under the same lock that numbers the new one and a newest
	// other than base (0 for none) is a *StaleBaseError, nothing written.
	InsertPolicy(ctx context.Context, base int64, actor, reason string, v Values, beforeCommit func(context.Context, Record) error) (Record, error)
}

// Projector writes a policy version to the KV bucket the hot path
// reads (WP-6 implements it on internal/bus).
type Projector interface {
	ProjectPolicy(ctx context.Context, r Record) error
}

// Service is api's handle on the policy: Load, Put and Current.
type Service struct {
	store     Store
	projector Projector
	counters  *core.Counters
	current   atomic.Pointer[Record]
}

// New is a Service; counters may be nil.
func New(store Store, projector Projector, counters *core.Counters) *Service {
	if counters == nil {
		counters = &core.Counters{}
	}
	return &Service{store: store, projector: projector, counters: counters}
}

// Counters are the service's counters.
func (s *Service) Counters() *core.Counters { return s.counters }

// Load reads the newest version from the database and makes it current.
// It returns ErrNoPolicy when none is stored: the caller decides whether
// to Put the defaults; nothing here pretends a version exists.
func (s *Service) Load(ctx context.Context) (Record, error) {
	r, err := s.store.NewestPolicy(ctx)
	if err != nil {
		return Record{}, err
	}
	if err := r.Values.Validate(); err != nil {
		return Record{}, fmt.Errorf("stored policy version %d does not validate: %w", r.Version, err)
	}
	s.current.Store(&r)
	return r, nil
}

// Put validates v and stores it as a new version, whatever version is
// in force (PutOn with AnyBase).
func (s *Service) Put(ctx context.Context, actor, reason string, v Values) (Record, error) {
	return s.PutOn(ctx, AnyBase, actor, reason, v)
}

// PutOn validates v and stores it as a new version made on version base
// (0 for the defaults): the row, its audit event and the KV projection
// are written in one transaction. When another version was stored since
// base, decided under the policy lock, nothing is written and the error
// is a *StaleBaseError; two changes made on the same version never both
// pass. When the projection fails the transaction rolls back, no row is
// left and the error is a *ProjectionError (503-shaped, B-09).
func (s *Service) PutOn(ctx context.Context, base int64, actor, reason string, v Values) (Record, error) {
	errs := []error{v.Validate()}
	if actor == "" {
		errs = append(errs, &core.FieldError{Field: "actor", Reason: "required"})
	}
	if reason == "" {
		errs = append(errs, &core.FieldError{Field: "reason", Reason: "required"})
	}
	if err := errors.Join(errs...); err != nil {
		return Record{}, err
	}
	r, err := s.store.InsertPolicy(ctx, base, actor, reason, v, func(ctx context.Context, r Record) error {
		if err := s.projector.ProjectPolicy(ctx, r); err != nil {
			s.counters.Inc(CounterProjectionFailed)
			return &ProjectionError{Bucket: BucketPolicy, Err: err}
		}
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	s.counters.Inc(CounterPut)
	s.current.Store(&r)
	return r, nil
}

// Current is the version loaded or put last, and false before either.
func (s *Service) Current() (Record, bool) {
	r := s.current.Load()
	if r == nil {
		return Record{}, false
	}
	return *r, true
}
