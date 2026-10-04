package coordination

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// ANSP is the ANSP's coordination inbox (Client).
type ANSP interface {
	// Submit posts one notice's bytes and returns the receipt.
	Submit(ctx context.Context, body []byte) (Receipt, error)
	// Get reads a notice's state by its ack_id.
	Get(ctx context.Context, ackID string) (NoticeState, error)
}

// PermanentError is an answer the ANSP will give again: 409
// (notice_ref_reused: this ref was sent with another body) or another
// refusal of the request itself. The notice fails for good.
type PermanentError struct {
	Status int
	Detail string
}

func (e *PermanentError) Error() string {
	return fmt.Sprintf("the ANSP refused the notice %d: %s", e.Status, e.Detail)
}

// RetryableError is an answer worth trying again later (unreachable,
// 5xx, 429, 401 while a token is renewed); RetryAfter is the ANSP's
// Retry-After when it gave one.
type RetryableError struct {
	Status     int
	Detail     string
	RetryAfter time.Duration
}

func (e *RetryableError) Error() string {
	if e.Status == 0 {
		return "the ANSP was not reached: " + e.Detail
	}
	return fmt.Sprintf("the ANSP answered %d: %s", e.Status, e.Detail)
}

// Counters of the Sender.
const (
	CounterSent           = "coordination_notices_received_by_ansp"
	CounterRepeats        = "coordination_notices_repeat_receipts"
	CounterRetried        = "coordination_notices_retried"
	CounterFailed         = "coordination_notices_failed"
	CounterConflict       = "coordination_notices_conflict"
	CounterGaveUp         = "coordination_notices_gave_up"
	CounterAcknowledged   = "coordination_notices_acknowledged"
	CounterEscalated      = "coordination_notices_escalated"
	CounterPollFailed     = "coordination_polls_failed"
	CounterStoreFailed    = "coordination_store_failed"
	CounterNotConfigured  = "coordination_ansp_not_configured"
	CounterPollsExhausted = "coordination_polls_stopped"
)

// Defaults of the Sender.
const (
	DefaultSendBatch   = 8
	DefaultLease       = 60 * time.Second
	DefaultMaxAttempts = 100
	DefaultBackoffMin  = time.Second
	DefaultBackoffMax  = 60 * time.Second
	// MaxRetryAfter bounds how long an ANSP's Retry-After defers a try.
	MaxRetryAfter = 5 * time.Minute
	// EscalatedPollMin is the slowest poll of an escalated notice, and
	// EscalatedPollFor how long after its escalation it is still read.
	EscalatedPollMin = 60 * time.Second
	EscalatedPollFor = time.Hour
)

// Sender works the outbox: it posts every due notice after its commit
// and reads back the acknowledgements (see the package documentation).
type Sender struct {
	Store Store
	// ANSP is nil when USSP_ANSP_BASE_URL is not set: nothing is claimed,
	// the notices stay pending and Probe says why.
	ANSP        ANSP
	Policy      func() policy.Values
	Counters    *core.Counters
	Logger      *slog.Logger
	Batch       int
	Lease       time.Duration
	MaxAttempts int
	BackoffMin  time.Duration
	BackoffMax  time.Duration
}

func (s *Sender) logger() *slog.Logger {
	if s.Logger == nil {
		return obs.Discard()
	}
	return s.Logger
}

func (s *Sender) count(name string) {
	if s.Counters != nil {
		s.Counters.Inc(name)
	}
}

func (s *Sender) policy() policy.Values {
	if s.Policy == nil {
		return policy.Defaults()
	}
	return s.Policy()
}

func (s *Sender) maxAttempts() int {
	if s.MaxAttempts <= 0 {
		return DefaultMaxAttempts
	}
	return s.MaxAttempts
}

// Backoff is the wait before try attempts+1: BackoffMin doubled per try
// already made, at most BackoffMax.
func (s *Sender) Backoff(attempts int) time.Duration {
	lo, hi := s.BackoffMin, s.BackoffMax
	if lo <= 0 {
		lo = DefaultBackoffMin
	}
	if hi <= 0 {
		hi = DefaultBackoffMax
	}
	d := lo
	for i := 1; i < attempts && d < hi; i++ {
		d *= 2
	}
	return min(d, hi)
}

// Run sends and polls every every until ctx ends; the send loop and the
// poll loop run apart, so a slow read never holds a notice back.
func (s *Sender) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultSweepEvery
	}
	var wg sync.WaitGroup
	loop := func(fn func(context.Context) int) {
		defer wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			fn(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}
	wg.Add(2)
	go loop(s.SendDue)
	go loop(func(ctx context.Context) int { return s.EscalateDue(ctx) + s.PollDue(ctx) })
	wg.Wait()
}

// SendDue claims the due notices and posts them, each on its own and in
// parallel (one per intent: the claim takes an intent's oldest pending
// notice only), and returns how many the ANSP received.
func (s *Sender) SendDue(ctx context.Context) int {
	if s.ANSP == nil {
		return 0
	}
	n := s.Batch
	if n <= 0 {
		n = DefaultSendBatch
	}
	lease := s.Lease
	if lease <= 0 {
		lease = DefaultLease
	}
	qs, err := s.Store.Claim(ctx, n, lease)
	if err != nil {
		if ctx.Err() == nil {
			s.count(CounterStoreFailed)
			s.logger().LogAttrs(ctx, slog.LevelWarn, "coordination outbox not read", obs.Err(err))
		}
		return 0
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	sent := 0
	for _, q := range qs {
		wg.Go(func() {
			if s.send(ctx, q) {
				mu.Lock()
				sent++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return sent
}

// send posts one claimed notice and records what the ANSP said. The
// store calls run on a context of their own: an answer the ANSP gave is
// recorded even when the process is stopping.
func (s *Sender) send(ctx context.Context, q Queued) bool {
	attrs := []slog.Attr{obs.IntentID(q.IntentID), slog.String("kind", string(q.Kind)), slog.String("notice_ref", q.Ref), slog.Int("attempt", q.Attempts)}
	r, err := s.ANSP.Submit(ctx, q.Body)
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var perm *PermanentError
	var retry *RetryableError
	switch {
	case err == nil:
		var poll time.Duration
		if AckRequired(q.Kind) {
			poll = seconds(s.policy().ATSAckPollS)
		}
		if err := s.Store.Received(sctx, q.ID, r, poll); err != nil {
			// The lease runs out and the notice is posted again: the ANSP
			// answers the repeat with the same receipt (200).
			s.count(CounterStoreFailed)
			s.logger().LogAttrs(ctx, slog.LevelError, "the ANSP's receipt not stored; the notice is posted again after its lease",
				append(attrs, slog.String("ack_id", r.AckID), obs.Err(err))...)
			return false
		}
		s.count(CounterSent)
		if r.Repeat {
			s.count(CounterRepeats)
		}
		s.logger().LogAttrs(ctx, slog.LevelInfo, "Annex V notice received by the ANSP", append(attrs, slog.String("ack_id", r.AckID), slog.Bool("repeat", r.Repeat))...)
		return true
	case errors.As(err, &perm):
		if perm.Status == 409 {
			s.count(CounterConflict)
		}
		s.fail(sctx, q, err.Error(), attrs)
		return false
	case q.Attempts >= s.maxAttempts():
		s.count(CounterGaveUp)
		s.fail(sctx, q, fmt.Sprintf("gave up after %d tries: %v", q.Attempts, err), attrs)
		return false
	default:
		backoff := s.Backoff(q.Attempts)
		if errors.As(err, &retry) && retry.RetryAfter > backoff {
			backoff = min(retry.RetryAfter, MaxRetryAfter)
		}
		s.count(CounterRetried)
		if serr := s.Store.Retry(sctx, q.ID, err.Error(), backoff); serr != nil {
			s.count(CounterStoreFailed)
			s.logger().LogAttrs(ctx, slog.LevelError, "a failed try not stored; the notice is posted again after its lease", append(attrs, obs.Err(serr))...)
		}
		s.logger().LogAttrs(ctx, slog.LevelWarn, "Annex V notice not received by the ANSP; tried again later",
			append(attrs, slog.Duration("backoff", backoff), obs.Err(err))...)
		return false
	}
}

func (s *Sender) fail(ctx context.Context, q Queued, cause string, attrs []slog.Attr) {
	s.count(CounterFailed)
	if err := s.Store.Fail(ctx, q.ID, cause); err != nil {
		s.count(CounterStoreFailed)
		s.logger().LogAttrs(ctx, slog.LevelError, "a refused notice not marked failed; it is posted again after its lease", append(attrs, obs.Err(err))...)
		return
	}
	s.logger().LogAttrs(ctx, slog.LevelError, "Annex V notice failed for good; it is on the console", append(attrs, slog.String("cause", cause))...)
}

// EscalateDue escalates the notices left unacknowledged past the
// policy's ats_ack_escalate_s and returns how many.
func (s *Sender) EscalateDue(ctx context.Context) int {
	es, err := s.Store.Escalate(ctx, seconds(s.policy().ATSAckEscalateS))
	if err != nil {
		if ctx.Err() == nil {
			s.count(CounterStoreFailed)
			s.logger().LogAttrs(ctx, slog.LevelWarn, "coordination escalation not run", obs.Err(err))
		}
		return 0
	}
	for i := range es {
		e := &es[i]
		s.count(CounterEscalated)
		s.logger().LogAttrs(ctx, slog.LevelError, "the ANSP has not acknowledged the notice: escalated on the console",
			obs.IntentID(e.IntentID), slog.String("kind", e.Kind), slog.String("notice_ref", e.NoticeRef), slog.Float64("age_s", e.AgeS))
	}
	return len(es)
}

// PollDue reads back the due acknowledgements and returns how many were
// acknowledged.
func (s *Sender) PollDue(ctx context.Context) int {
	if s.ANSP == nil {
		return 0
	}
	ps, err := s.Store.DuePolls(ctx, DefaultBatch)
	if err != nil {
		if ctx.Err() == nil {
			s.count(CounterStoreFailed)
			s.logger().LogAttrs(ctx, slog.LevelWarn, "coordination polls not read", obs.Err(err))
		}
		return 0
	}
	pol := s.policy()
	acked := 0
	for _, p := range ps {
		if ctx.Err() != nil {
			break
		}
		if s.poll(ctx, p, pol) {
			acked++
		}
	}
	return acked
}

func (s *Sender) poll(ctx context.Context, p Polled, pol policy.Values) bool {
	attrs := []slog.Attr{slog.String("notice_ref", p.Ref), slog.String("ack_id", p.AckID)}
	st, err := s.ANSP.Get(ctx, p.AckID)
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err == nil && st.State == "acknowledged" {
		if err := s.Store.Acknowledged(sctx, p.ID, st); err != nil {
			s.count(CounterStoreFailed)
			s.logger().LogAttrs(ctx, slog.LevelError, "the ANSP's acknowledgement not stored; read again", append(attrs, obs.Err(err))...)
			return false
		}
		s.count(CounterAcknowledged)
		s.logger().LogAttrs(ctx, slog.LevelInfo, "Annex V notice acknowledged at the ANSP", append(attrs, slog.String("acknowledged_by", st.AcknowledgedBy))...)
		return true
	}
	if err != nil {
		s.count(CounterPollFailed)
		s.logger().LogAttrs(ctx, slog.LevelWarn, "the ANSP's notice state not read; read again", append(attrs, obs.Err(err))...)
	}
	next := NextPoll(p, pol)
	if next == 0 {
		s.count(CounterPollsExhausted)
		s.logger().LogAttrs(ctx, slog.LevelError, "the ANSP never acknowledged the notice; no longer read, still escalated on the console", attrs...)
	}
	if err := s.Store.Polled(sctx, p.ID, next); err != nil {
		s.count(CounterStoreFailed)
	}
	return false
}

// NextPoll is when to read p again after a read that found no
// acknowledgement: every ats_ack_poll_s until it is escalated, then at
// most once a minute for EscalatedPollFor, then never (0). The age is
// the database's (Polled.SinceReceived).
func NextPoll(p Polled, pol policy.Values) time.Duration {
	every := seconds(pol.ATSAckPollS)
	if p.State != "escalated" {
		return every
	}
	if p.SinceReceived > seconds(pol.ATSAckEscalateS)+EscalatedPollFor {
		return 0
	}
	return max(every, EscalatedPollMin)
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
