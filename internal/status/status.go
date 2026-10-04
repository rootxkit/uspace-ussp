// Package status is this USSP's operating-status notices to the
// authority (Reg. (EU) 2021/664 Art. 7(6); spec 02 F7; brief WP-15):
// the start of operations, once, when the operator confirms it on the
// console with the certificate id configured (USSP_CERTIFICATE_ID);
// cease and restart on request. Each notice is stored first and sent
// after its commit to POST /v1/certificates/{id}/status (the client
// generated from the pinned api/clients/authority.yaml, scope
// certificates.status, aud the authority's host), with this USSP's
// reference: the authority answers a retry of the same reference and
// state with the notice it recorded first, so a notice is recorded once
// however often it is sent. A stored start is never asked for again:
// a restart of the process, or a second confirmation, finds it.
// Notices are retried with backoff while the authority is unreachable,
// fail for good on a refusal (409: the authority's status does not admit
// it), and every state is on the console and /readyz.
package status

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Kinds of notice (Art. 7(6)) and the authority's states for them.
const (
	KindStart   = "start"
	KindCease   = "cease"
	KindRestart = "restart"
)

// AuthorityState is the state the authority's notice names for kind.
func AuthorityState(kind string) string {
	switch kind {
	case KindStart:
		return "started"
	case KindCease:
		return "ceased"
	case KindRestart:
		return "restarted"
	}
	return ""
}

// Notice is one stored notice.
type Notice struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	At            time.Time  `json:"at"`
	CertificateID string     `json:"certificate_id"`
	Reference     string     `json:"reference"`
	RequestedBy   string     `json:"requested_by"`
	State         string     `json:"state"`
	Attempts      int        `json:"attempts"`
	NextAt        *time.Time `json:"next_at"`
	LastError     *string    `json:"last_error"`
	SubmittedAt   *time.Time `json:"submitted_at"`
	AuthorityRef  *string    `json:"authority_ref"`
	FailedAt      *time.Time `json:"failed_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Queued is a notice claimed for one try.
type Queued struct {
	ID            string
	Kind          string
	At            time.Time
	CertificateID string
	Reference     string
	Attempts      int
}

// ErrStartExists is a second start of one certificate.
var ErrStartExists = errors.New("a start notice is stored for this certificate")

// Store is operating_status_notices.
type Store interface {
	// Notices are up to n notices of the certificate, newest first.
	Notices(ctx context.Context, certificateID string, n int) ([]Notice, error)
	// Insert stores a notice (ErrStartExists for a second start).
	Insert(ctx context.Context, kind, certificateID, reference, requestedBy string) (Notice, error)
	Claim(ctx context.Context, n int, lease time.Duration) ([]Queued, error)
	Delivered(ctx context.Context, id, authorityRef string) error
	Retry(ctx context.Context, id, cause string, backoff time.Duration) error
	Fail(ctx context.Context, id, cause string) error
}

// Authority is the authority's operating-status inbox (Client).
type Authority interface {
	Post(ctx context.Context, q Queued) (authorityRef string, err error)
}

// PermanentError is a refusal the authority will repeat.
type PermanentError struct {
	Status int
	Detail string
}

func (e *PermanentError) Error() string {
	return fmt.Sprintf("the authority refused the notice %d: %s", e.Status, e.Detail)
}

// RequestError is a request the stored notices do not admit.
type RequestError struct{ Reason string }

func (e *RequestError) Error() string { return e.Reason }

// Counters of the Service.
const (
	CounterRequested   = "status_notices_requested"
	CounterDelivered   = "status_notices_delivered"
	CounterRetried     = "status_notices_retried"
	CounterFailed      = "status_notices_failed"
	CounterStoreFailed = "status_store_failed"
)

// Defaults of the Service.
const (
	DefaultEvery       = 5 * time.Second
	DefaultMaxAttempts = 100
	DefaultLease       = 60 * time.Second
	BackoffMin         = 5 * time.Second
	BackoffMax         = 10 * time.Minute
	// MaxListed bounds the notices listed at once.
	MaxListed = 100
)

// DepStatus is the readiness dependency of the notices.
const DepStatus = "operating_status"

// Service stores, sends and reports the notices.
type Service struct {
	Store Store
	// Authority is nil without an authority client (USSP_AUTHORITY_BASE_URL
	// and an outgoing token client): notices are stored, not sent.
	Authority     Authority
	CertificateID string
	SystemID      string
	MaxAttempts   int
	Counters      *core.Counters
	Logger        *slog.Logger
}

func (s *Service) logger() *slog.Logger {
	if s.Logger == nil {
		return obs.Discard()
	}
	return s.Logger
}

func (s *Service) count(name string) {
	if s.Counters != nil {
		s.Counters.Inc(name)
	}
}

func newReference(systemID, kind string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return systemID + ":status:" + kind + ":" + hex.EncodeToString(b[:]), nil
}

// lastStanding is the newest notice that is not failed.
func lastStanding(ns []Notice) *Notice {
	for i := range ns {
		if ns[i].State != "failed" {
			return &ns[i]
		}
	}
	return nil
}

// Request stores a notice of kind asked by staffID: a start once per
// certificate (a second request answers the stored one), a cease after a
// start or a restart, a restart after a cease; asking again for the
// state the last notice already gives answers that notice (created
// false). It is sent after the commit.
func (s *Service) Request(ctx context.Context, staffID, kind string) (Notice, bool, error) {
	if s.CertificateID == "" {
		return Notice{}, false, &RequestError{"USSP_CERTIFICATE_ID is not set: no operating-status notice can name the certificate"}
	}
	if AuthorityState(kind) == "" {
		return Notice{}, false, &RequestError{"unknown kind " + kind + " (start, cease or restart)"}
	}
	ns, err := s.Store.Notices(ctx, s.CertificateID, MaxListed)
	if err != nil {
		return Notice{}, false, err
	}
	last := lastStanding(ns)
	switch {
	case last != nil && last.Kind == kind:
		return *last, false, nil
	case kind == KindStart:
		for i := range ns {
			if ns[i].Kind == KindStart && ns[i].State != "failed" {
				return ns[i], false, nil
			}
		}
	case kind == KindCease && (last == nil || last.Kind == KindCease):
		return Notice{}, false, &RequestError{"a cease follows a start or a restart"}
	case kind == KindRestart && (last == nil || last.Kind != KindCease):
		return Notice{}, false, &RequestError{"a restart follows a cease"}
	}
	ref, err := newReference(s.SystemID, kind)
	if err != nil {
		return Notice{}, false, err
	}
	n, err := s.Store.Insert(ctx, kind, s.CertificateID, ref, staffID)
	if errors.Is(err, ErrStartExists) {
		ns, err := s.Store.Notices(ctx, s.CertificateID, MaxListed)
		if err != nil {
			return Notice{}, false, err
		}
		for i := range ns {
			if ns[i].Kind == KindStart && ns[i].State != "failed" {
				return ns[i], false, nil
			}
		}
		return Notice{}, false, ErrStartExists
	}
	if err != nil {
		return Notice{}, false, err
	}
	s.count(CounterRequested)
	s.logger().LogAttrs(ctx, slog.LevelInfo, "operating-status notice stored for the authority", slog.String("kind", kind),
		slog.String("reference", ref), slog.String("requested_by", staffID))
	return n, true, nil
}

// List is the certificate's notices, newest first.
func (s *Service) List(ctx context.Context) ([]Notice, error) {
	if s.CertificateID == "" {
		return []Notice{}, nil
	}
	return s.Store.Notices(ctx, s.CertificateID, MaxListed)
}

// Backoff is the wait before try attempts+1.
func Backoff(attempts int) time.Duration {
	d := BackoffMin
	for i := 1; i < attempts && d < BackoffMax; i++ {
		d *= 2
	}
	return min(d, BackoffMax)
}

// Run sends the due notices every every until ctx ends.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.SendDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SendDue sends the due notices (one certificate's in order) and returns
// how many the authority recorded. Without an Authority nothing is sent.
func (s *Service) SendDue(ctx context.Context) int {
	if s.Authority == nil {
		return 0
	}
	qs, err := s.Store.Claim(ctx, 4, DefaultLease)
	if err != nil {
		if ctx.Err() == nil {
			s.count(CounterStoreFailed)
		}
		return 0
	}
	maxTries := s.MaxAttempts
	if maxTries <= 0 {
		maxTries = DefaultMaxAttempts
	}
	n := 0
	for _, q := range qs {
		ref, err := s.Authority.Post(ctx, q)
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		var perm *PermanentError
		switch {
		case err == nil:
			if err := s.Store.Delivered(sctx, q.ID, ref); err != nil {
				s.count(CounterStoreFailed)
			} else {
				n++
				s.count(CounterDelivered)
				s.logger().LogAttrs(ctx, slog.LevelInfo, "operating-status notice recorded by the authority", slog.String("kind", q.Kind),
					slog.String("reference", q.Reference), slog.String("authority_ref", ref))
			}
		case errors.As(err, &perm), q.Attempts >= maxTries:
			s.count(CounterFailed)
			cause := err.Error()
			if perm == nil {
				cause = fmt.Sprintf("gave up after %d tries: %v", q.Attempts, err)
			}
			if err := s.Store.Fail(sctx, q.ID, cause); err != nil {
				s.count(CounterStoreFailed)
			}
			s.logger().LogAttrs(ctx, slog.LevelError, "operating-status notice failed for good; it is on the console", slog.String("kind", q.Kind),
				slog.String("reference", q.Reference), slog.String("cause", cause))
		default:
			s.count(CounterRetried)
			if err := s.Store.Retry(sctx, q.ID, err.Error(), Backoff(q.Attempts)); err != nil {
				s.count(CounterStoreFailed)
			}
			s.logger().LogAttrs(ctx, slog.LevelWarn, "operating-status notice not recorded yet; tried again later", slog.String("kind", q.Kind), obs.Err(err))
		}
		cancel()
	}
	return n
}

// Probe is the readiness of the notices: up without a certificate id
// (no certified operation to notify; said), degraded with one before a
// start is recorded, without an authority client, while a notice is
// pending and when the last one failed; up when the last notice is
// recorded (operating, or ceased since); unknown when they cannot be
// read.
func (s *Service) Probe() obs.Probe {
	return func(ctx context.Context) (obs.State, string) {
		if s.CertificateID == "" {
			return obs.StateUp, "USSP_CERTIFICATE_ID is not set: no operating-status notice can be sent (Art. 7(6))"
		}
		ns, err := s.Store.Notices(ctx, s.CertificateID, MaxListed)
		if err != nil {
			return obs.StateUnknown, "the operating-status notices cannot be read: " + err.Error()
		}
		if len(ns) == 0 {
			return obs.StateDegraded, "no start notice yet: confirm the start of operations on the console"
		}
		n := ns[0]
		detail := fmt.Sprintf("last notice %s %s", n.Kind, n.State)
		if n.LastError != nil {
			detail += ": " + *n.LastError
		}
		switch {
		case s.Authority == nil:
			return obs.StateDegraded, "no authority client (USSP_AUTHORITY_BASE_URL, USSP_TOKEN_ISSUERS): notices are stored, not sent; " + detail
		case n.State != "delivered":
			return obs.StateDegraded, detail
		}
		return obs.StateUp, detail
	}
}
