package bus

import "github.com/nats-io/nats.go"

// Listen delivers every core NATS message on subject (a filter) to
// handle with its subject and data, until stop is called: for what a
// process needs only live, with no replay (monitor's flight ends; a
// test's view of the bus). A message is handed over as it arrives, on
// the connection's delivery goroutine, so handle must not block.
func (c *Conn) Listen(subject string, handle func(subject string, data []byte)) (stop func(), err error) {
	sub, err := c.Subscribe(subject, func(m *nats.Msg) { handle(m.Subject, m.Data) })
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}
