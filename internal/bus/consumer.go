package bus

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/rootxkit/uspace-core/core"
)

// PullSpec is a durable pull consumer with explicit ack and a bounded
// number of messages delivered and not yet acknowledged (spec 05 §5).
type PullSpec struct {
	Durable       string
	FilterSubject string
	// MaxAckPending is required: an unbounded consumer is refused.
	MaxAckPending int
	// AckWait defaults to 30 s.
	AckWait time.Duration
}

// PullConsumer creates or updates the durable pull consumer of spec on
// the stream, which it creates from t when it is missing. Deliveries are
// never given up on (MaxDeliver -1): the consumer decides what is
// dropped and records it.
func PullConsumer(ctx context.Context, js jetstream.JetStream, t Topology, stream string, spec PullSpec) (jetstream.Stream, jetstream.Consumer, error) {
	if spec.MaxAckPending <= 0 {
		return nil, nil, core.Fieldf("max_ack_pending", "consumer %s: must be bounded (> 0)", spec.Durable)
	}
	if spec.AckWait <= 0 {
		spec.AckWait = 30 * time.Second
	}
	cfg, ok := t.Stream(stream)
	if !ok {
		return nil, nil, core.Fieldf("stream", "%q is not in the topology", stream)
	}
	if _, err := Ensure(ctx, js, Topology{Streams: []jetstream.StreamConfig{cfg}}); err != nil {
		return nil, nil, err
	}
	s, err := js.Stream(ctx, stream)
	if err != nil {
		return nil, nil, fmt.Errorf("stream %s: %w", stream, err)
	}
	c, err := s.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable: spec.Durable, FilterSubject: spec.FilterSubject, AckPolicy: jetstream.AckExplicitPolicy,
		AckWait: spec.AckWait, MaxAckPending: spec.MaxAckPending, MaxDeliver: -1,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("consumer %s: %w", spec.Durable, err)
	}
	return s, c, nil
}

// PullOpener is a StreamSource's Open for the pull consumer spec on the
// stream named in t.
func PullOpener(js jetstream.JetStream, t Topology, stream string, spec PullSpec) func(context.Context) (jetstream.Stream, jetstream.Consumer, error) {
	return func(ctx context.Context) (jetstream.Stream, jetstream.Consumer, error) {
		return PullConsumer(ctx, js, t, stream, spec)
	}
}
