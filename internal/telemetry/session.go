package telemetry

import (
	"slices"
	"strconv"
	"sync"
	"time"
)

// maxOpenSeqs bounds the samples of one serial a session tracks as not
// yet settled (E-10); beyond it the oldest is let go and counted.
const maxOpenSeqs = 10_000

// ackState is the acknowledgement of one serial on one session: the
// highest seq settled, and the seqs taken but not settled yet (on their
// way to the bus, or not handed and to be sent again).
type ackState struct {
	top    int64
	hasTop bool
	open   map[int64]bool
}

// acked is the acknowledgement of the serial on this session: every
// sample of it sent on this connection with a seq up to the value is
// settled (cumulative over this connection's samples only; a sample
// sent on another connection is acknowledged there). -1 when nothing is
// settled yet.
func (a *ackState) acked() int64 {
	if !a.hasTop {
		return -1
	}
	if len(a.open) == 0 {
		return a.top
	}
	lowest := int64(-1)
	for s := range a.open {
		if lowest < 0 || s < lowest {
			lowest = s
		}
	}
	return min(a.top, lowest-1)
}

// Session is one WS /v1/telemetry connection (B-14): its id and the
// order it was opened in, its client, what it was told, and the aircraft
// it streams. Safe for concurrent use.
type Session struct {
	ID       uint64
	Order    uint64
	ClientID string
	Opened   time.Time

	mu       sync.Mutex
	acks     map[string]*ackState
	outcomes map[string]uint64
	accepted uint64
	backlog  uint64 // accepted samples that are history (T-04)
	overflow uint64
	aircraft map[*aircraft]bool
	// window counts accepted samples per status period for the rate.
	windowN     uint64
	windowStart time.Time
	rate        float64
	// poke wakes the status writer after a refusal.
	poke chan struct{}
}

// NewSession is a session of client opened at now, with the next id of
// in.
func (in *Ingestor) NewSession(clientID string, now time.Time) *Session {
	id := in.orders.take()
	return &Session{ID: id, Order: id, ClientID: clientID, Opened: now, acks: map[string]*ackState{}, outcomes: map[string]uint64{},
		aircraft: map[*aircraft]bool{}, windowStart: now, poke: make(chan struct{}, 1)}
}

// Poke is the channel the status writer waits on beside its ticker.
func (s *Session) Poke() <-chan struct{} { return s.poke }

func (s *Session) wake() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

func (s *Session) ack(sn string) *ackState {
	a := s.acks[sn]
	if a == nil {
		a = &ackState{open: map[int64]bool{}}
		s.acks[sn] = a
	}
	return a
}

// pending marks seq of sn as taken and on its way (before it is handed
// to the outbox, so its settlement cannot come first).
func (s *Session) pending(sn string, seq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.ack(sn)
	if len(a.open) >= maxOpenSeqs {
		lowest := int64(-1)
		for q := range a.open {
			if lowest < 0 || q < lowest {
				lowest = q
			}
		}
		delete(a.open, lowest)
		s.overflow++
	}
	a.open[seq] = true
}

func (s *Session) settle(sn string, seq int64) {
	a := s.ack(sn)
	delete(a.open, seq)
	if !a.hasTop || seq > a.top {
		a.top, a.hasTop = seq, true
	}
}

// handed settles seq of sn when it reached the bus; one that did not
// stays open (not acknowledged: the client sends it again).
func (s *Session) handed(sn string, seq int64, handed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if handed {
		s.settle(sn, seq)
	}
}

// outcome records the immediate outcome of one sample.
func (s *Session) outcome(r Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes[r.Reason]++
	switch {
	case r.Reason == OutcomeAccepted:
		s.accepted++
		s.windowN++
		if r.Backlog {
			s.backlog++
		}
	case settled(r.Reason):
		if r.Serial != "" {
			s.settle(r.Serial, r.Seq)
		}
		if r.Reason != OutcomeDuplicate {
			s.wake()
		}
	default:
		if r.Serial != "" {
			a := s.ack(r.Serial)
			a.open[r.Seq] = true
		}
		s.wake()
	}
}

// own records that s streams ac.
func (s *Session) own(ac *aircraft) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aircraft[ac] = true
}

func (s *Session) owned() []*aircraft {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*aircraft, 0, len(s.aircraft))
	for ac := range s.aircraft {
		out = append(out, ac)
	}
	return out
}

// SessionStatus is what a status frame says of the session.
type SessionStatus struct {
	Accepted uint64
	Backlog  uint64
	Refused  uint64
	Dropped  uint64
	Outcomes map[string]uint64
	Rate     float64
	AckedSeq map[string]int64
	Overflow uint64
}

// Status is the session's state at now; the rate is the accepted
// samples per second since the previous call.
func (s *Session) Status(now time.Time) SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := now.Sub(s.windowStart).Seconds(); d > 0 {
		s.rate = float64(s.windowN) / d
		s.windowN, s.windowStart = 0, now
	}
	st := SessionStatus{Accepted: s.accepted, Backlog: s.backlog, Outcomes: map[string]uint64{}, Rate: s.rate, AckedSeq: map[string]int64{}, Overflow: s.overflow}
	for k, v := range s.outcomes {
		st.Outcomes[k] = v
		switch {
		case k == OutcomeAccepted || k == OutcomeDuplicate || k == OutcomeDuplicatePending:
		case isDropped(k):
			st.Dropped += v
		default:
			st.Refused += v
		}
	}
	serials := make([]string, 0, len(s.acks))
	for sn := range s.acks {
		serials = append(serials, sn)
	}
	slices.Sort(serials)
	for _, sn := range serials {
		if a := s.acks[sn].acked(); a >= 0 {
			st.AckedSeq[sn] = a
		}
	}
	return st
}

func isDropped(reason string) bool { return reason == DroppedRate || reason == DroppedQueueFull }

// connectionID is the session's id as the status frame names it.
func (s *Session) connectionID() string { return "telemetry-" + strconv.FormatUint(s.ID, 10) }
