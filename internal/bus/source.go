package bus

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Msg is what a consumer of a stream needs of a delivered message, so
// tests drive a consumer without a server.
type Msg interface {
	Data() []byte
	Subject() string
	// StreamSeq is the message's sequence in its stream.
	StreamSeq() (uint64, error)
	Ack() error
	Nak() error
	InProgress() error
}

// jsMsg is a delivered JetStream message as a Msg.
type jsMsg struct{ jetstream.Msg }

// StreamSeq implements Msg.
func (m jsMsg) StreamSeq() (uint64, error) {
	meta, err := m.Metadata()
	if err != nil {
		return 0, err
	}
	return meta.Sequence.Stream, nil
}

// Jump is a step in the stream sequences the consumer was delivered:
// the sequences strictly between After and Before were not.
type Jump struct{ After, Before uint64 }

// Hole is the part of a jump the stream no longer holds.
type Hole struct {
	FromSeq, ToSeq uint64
	// Count is how many sequences in [FromSeq, ToSeq] are gone.
	Count uint64
}

// Source is one stream's durable consumer as tsdb-writer reads it.
type Source interface {
	// Fetch returns up to n messages, waiting at most wait for the first.
	Fetch(ctx context.Context, n int, wait time.Duration) ([]Msg, error)
	// AckFloor is the stream sequence at and below which every message
	// of the consumer is acknowledged.
	AckFloor(ctx context.Context) (uint64, error)
	// Holes reports, per jump, the sequences the stream no longer holds.
	Holes(ctx context.Context, jumps []Jump) ([]Hole, error)
}

// StreamSource is a durable pull consumer over a whole stream, opened on
// first use and again after a failure (NATS may be down at start,
// B-08). One consumer per stream and over the whole stream: a step in
// the sequences it is delivered is a message the stream removed, never
// a message of another consumer's filter, so a quiet stream records no
// gap (the false gap of a filtered consumer on a shared stream).
type StreamSource struct {
	// Open returns the stream and the consumer.
	Open func(ctx context.Context) (jetstream.Stream, jetstream.Consumer, error)
	// MaxDeletedDetails bounds the interior deletes read to count a hole
	// (E-10); beyond it a hole counts the head losses only.
	MaxDeletedDetails int

	mu       sync.Mutex
	stream   jetstream.Stream
	consumer jetstream.Consumer
}

func (s *StreamSource) get(ctx context.Context) (jetstream.Stream, jetstream.Consumer, error) {
	s.mu.Lock()
	st, c := s.stream, s.consumer
	s.mu.Unlock()
	if c != nil {
		return st, c, nil
	}
	st, c, err := s.Open(ctx)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	s.stream, s.consumer = st, c
	s.mu.Unlock()
	return st, c, nil
}

func (s *StreamSource) reset() {
	s.mu.Lock()
	s.stream, s.consumer = nil, nil
	s.mu.Unlock()
}

// Fetch implements Source.
func (s *StreamSource) Fetch(ctx context.Context, n int, wait time.Duration) ([]Msg, error) {
	_, c, err := s.get(ctx)
	if err != nil {
		return nil, err
	}
	b, err := c.Fetch(n, jetstream.FetchMaxWait(wait))
	if err != nil {
		s.reset()
		return nil, err
	}
	var out []Msg
	for m := range b.Messages() {
		out = append(out, jsMsg{m})
	}
	if err := b.Error(); err != nil && !errors.Is(err, nats.ErrTimeout) && len(out) == 0 {
		s.reset()
		return nil, err
	}
	return out, nil
}

// AckFloor implements Source.
func (s *StreamSource) AckFloor(ctx context.Context) (uint64, error) {
	_, c, err := s.get(ctx)
	if err != nil {
		return 0, err
	}
	info, err := c.Info(ctx)
	if err != nil {
		s.reset()
		return 0, err
	}
	return info.AckFloor.Stream, nil
}

// Holes implements Source: limits and purges remove messages from the
// stream's head, so every sequence of a jump below the stream's first
// sequence is gone; interior deletes are read from the stream's deleted
// list when it is not longer than MaxDeletedDetails.
func (s *StreamSource) Holes(ctx context.Context, jumps []Jump) ([]Hole, error) {
	st, _, err := s.get(ctx)
	if err != nil {
		return nil, err
	}
	info, err := st.Info(ctx)
	if err != nil {
		s.reset()
		return nil, err
	}
	first := info.State.FirstSeq
	var deleted []uint64
	if info.State.NumDeleted > 0 && (s.MaxDeletedDetails <= 0 || info.State.NumDeleted <= s.MaxDeletedDetails) {
		di, err := st.Info(ctx, jetstream.WithDeletedDetails(true))
		if err != nil {
			return nil, err
		}
		deleted = di.State.Deleted
		first = max(first, di.State.FirstSeq)
	}
	return holesOf(jumps, first, deleted), nil
}

// holesOf computes, per jump, the sequences gone from a stream whose
// first sequence is first and whose interior deletes are deleted.
func holesOf(jumps []Jump, first uint64, deleted []uint64) []Hole {
	out := make([]Hole, len(jumps))
	for i, j := range jumps {
		var h Hole
		if end := min(j.Before, first); end > j.After+1 {
			h.FromSeq, h.ToSeq, h.Count = j.After+1, end-1, end-1-j.After
		}
		for _, d := range deleted {
			if d > j.After && d < j.Before && d >= first {
				if h.Count == 0 || d < h.FromSeq {
					h.FromSeq = d
				}
				h.ToSeq = max(h.ToSeq, d)
				h.Count++
			}
		}
		out[i] = h
	}
	return out
}

// NeverDelivered is the range of stream sequences the stream removed
// before the consumer was delivered them: above the highest sequence the
// consumer was ever delivered and below the stream's first sequence. On a
// work queue (INGEST) that is every message aged out or purged unread,
// whichever replica reads the shared consumer, so a loss nobody saw is
// still counted (05 §5, B-13). from > to means none.
func (s *StreamSource) NeverDelivered(ctx context.Context) (from, to uint64, err error) {
	st, c, err := s.get(ctx)
	if err != nil {
		return 1, 0, err
	}
	ci, err := c.Info(ctx)
	if err != nil {
		s.reset()
		return 1, 0, err
	}
	si, err := st.Info(ctx)
	if err != nil {
		s.reset()
		return 1, 0, err
	}
	return neverDelivered(ci.Delivered.Stream, si.State.FirstSeq)
}

// neverDelivered is the range (delivered, first) exclusive at both ends.
func neverDelivered(delivered, first uint64) (from, to uint64, err error) {
	if first <= delivered+1 {
		return 1, 0, nil
	}
	return delivered + 1, first - 1, nil
}

var _ Source = (*StreamSource)(nil)
