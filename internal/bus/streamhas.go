package bus

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// StreamHas answers whether a stream holds a message on a subject, each
// call bounded by Timeout (DefaultKVTimeout): telemetry-ingest asks the
// FLIGHT stream whether a saved flight's ended fact was published
// before it takes the flight back (WP-19). The stream is opened at the
// first call and kept. Safe for concurrent use.
type StreamHas struct {
	JS      jetstream.JetStream
	Stream  string
	Timeout time.Duration

	mu sync.Mutex
	st jetstream.Stream
}

func (h *StreamHas) stream(ctx context.Context) (jetstream.Stream, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.st != nil {
		return h.st, nil
	}
	st, err := h.JS.Stream(ctx, h.Stream)
	if err != nil {
		return nil, err
	}
	h.st = st
	return st, nil
}

// Has reports whether the stream holds a message on subject. A stream
// that does not exist, or that cannot be read, is an error, never a
// "no".
func (h *StreamHas) Has(ctx context.Context, subject string) (bool, error) {
	t := h.Timeout
	if t <= 0 {
		t = DefaultKVTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	st, err := h.stream(ctx)
	if err != nil {
		return false, err
	}
	_, err = st.GetLastMsgForSubject(ctx, subject)
	if errors.Is(err, jetstream.ErrMsgNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
