package bus

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Replay delivers the messages of one stream from a moment on: an
// ordered consumer of Stream filtered to Subject, which follows NATS
// through reconnects on its own (rid-sp's window reads TRK this way, so
// a restart serves the last minute at once, B-05).
type Replay struct {
	JS      jetstream.JetStream
	Stream  string
	Subject string
}

// Open delivers every message from from on, then as they come, to
// handle, until stop is called.
func (r Replay) Open(ctx context.Context, from time.Time, handle func(data []byte)) (stop func(), err error) {
	cons, err := r.JS.OrderedConsumer(ctx, r.Stream, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{r.Subject}, DeliverPolicy: jetstream.DeliverByStartTimePolicy, OptStartTime: &from,
	})
	if err != nil {
		return nil, err
	}
	cc, err := cons.Consume(func(m jetstream.Msg) { handle(m.Data()) })
	if err != nil {
		return nil, err
	}
	return cc.Stop, nil
}
