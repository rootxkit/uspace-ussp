package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/rootxkit/uspace-core/core"
)

// Counter name prefixes of the publisher: published_<kind> and
// publish_failed_<kind>, the kind with "." as "_" (traffic_product).
const (
	CounterPublishedPrefix     = "published_"
	CounterPublishFailedPrefix = "publish_failed_"
)

// DefaultPublishTimeout bounds one JetStream publish.
const DefaultPublishTimeout = 2 * time.Second

// CorePublisher is the core NATS publish (*nats.Conn).
type CorePublisher interface {
	PublishMsg(m *nats.Msg) error
}

// Publisher sends this system's messages: core NATS for trk, man, peer,
// src and ctl (the hot path never waits; TRK, MAN and PEER capture
// them), JetStream for every other subject with the envelope's msg_id
// as Nats-Msg-Id, so a retried publish inside the duplicate window is
// stored once. Every publish is counted as published_<kind> or
// publish_failed_<kind>.
type Publisher struct {
	Core     CorePublisher
	JS       jetstream.JetStream
	Counters *core.Counters
	// Timeout bounds one JetStream publish (DefaultPublishTimeout).
	Timeout time.Duration
}

// NewPublisher is a publisher on c.
func NewPublisher(c *Conn, counters *core.Counters) *Publisher {
	if counters == nil {
		counters = &core.Counters{}
	}
	return &Publisher{Core: c.Conn, JS: c.JetStream(), Counters: counters}
}

func counterKind(kind string) string { return strings.ReplaceAll(kind, ".", "_") }

// Publish validates m's envelope and sends it on subject. A subject that
// does not parse, an envelope that does not validate or a message larger
// than MaxPayloadBytes is refused before anything is sent.
func (p *Publisher) Publish(ctx context.Context, subject string, m Enveloped) error {
	s, err := Parse(subject)
	if err != nil {
		p.Counters.Inc(CounterPublishFailedPrefix + "invalid_subject")
		return err
	}
	kind := counterKind(s.Kind)
	fail := func(err error) error {
		p.Counters.Inc(CounterPublishFailedPrefix + kind)
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	env := m.Head()
	if err := env.Validate(); err != nil {
		return fail(err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fail(err)
	}
	if err := p.send(ctx, subject, s.Kind, env.MsgID, data); err != nil {
		return fail(err)
	}
	p.Counters.Inc(CounterPublishedPrefix + kind)
	return nil
}

// PublishControl sends v as JSON on a control subject (ctl.*), core
// NATS: a version announcement, not an 04 message.
func (p *Publisher) PublishControl(subject string, v any) error {
	s, err := Parse(subject)
	if err != nil || s.Kind != KindCtl {
		p.Counters.Inc(CounterPublishFailedPrefix + "invalid_subject")
		return core.Fieldf("subject", "%q is not a control subject", subject)
	}
	data, err := json.Marshal(v)
	if err == nil {
		err = p.send(context.Background(), subject, KindCtl, "", data)
	}
	if err != nil {
		p.Counters.Inc(CounterPublishFailedPrefix + KindCtl)
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	p.Counters.Inc(CounterPublishedPrefix + KindCtl)
	return nil
}

func (p *Publisher) send(ctx context.Context, subject, kind, msgID string, data []byte) error {
	if len(data) > MaxPayloadBytes {
		return core.Fieldf("message", "%d bytes, more than %d", len(data), MaxPayloadBytes)
	}
	msg := nats.NewMsg(subject)
	msg.Data = data
	if !Durable(kind) {
		return p.Core.PublishMsg(msg)
	}
	msg.Header.Set(jetstream.MsgIDHeader, msgID)
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultPublishTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := p.JS.PublishMsg(pctx, msg)
	return err
}
