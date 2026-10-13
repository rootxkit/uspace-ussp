//go:build integration

package bus

import (
	"io"
	"log/slog"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blackhole forwards TCP to target until frozen; frozen, it keeps every
// connection open and forwards nothing either way: a silent partition,
// where no FIN or RST ever reaches the client.
type blackhole struct {
	ln     net.Listener
	target string
	frozen atomic.Bool
	mu     sync.Mutex
	conns  []net.Conn
}

func newBlackhole(t *testing.T, target string) *blackhole {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &blackhole{ln: ln, target: target}
	t.Cleanup(b.close)
	go b.serve()
	return b
}

func (b *blackhole) serve() {
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}
		up, err := net.DialTimeout("tcp", b.target, 2*time.Second)
		if err != nil {
			_ = c.Close()
			continue
		}
		b.mu.Lock()
		b.conns = append(b.conns, c, up)
		b.mu.Unlock()
		go b.pipe(up, c)
		go b.pipe(c, up)
	}
}

// pipe copies src to dst while not frozen; what arrives while frozen is
// dropped.
func (b *blackhole) pipe(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !b.frozen.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (b *blackhole) close() {
	_ = b.ln.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns {
		_ = c.Close()
	}
}

// Q35 review: a silent partition (NATS unreachable, nothing closing the
// connection) is seen within seconds. Without a ping interval the client
// noticed it only at nats.go's default, two minutes per ping and two
// pings out, and every consumer judged the silence as if the bus were
// up. With PingInterval and MaxPingsOut the link is down within
// (MaxPingsOut + 1) x PingInterval of the partition, and up again once
// it ends (the presence pair).
func TestIntegrationSilentPartitionIsSeenWithinSeconds(t *testing.T) {
	u, err := url.Parse(natsURL(t))
	if err != nil {
		t.Fatal("USSP_TEST_NATS_URL does not parse")
	}
	b := newBlackhole(t, u.Host)
	u.Host = b.ln.Addr().String()
	c, err := Connect(u.String(), "", "bus-partition-test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal("connect through the blackhole failed")
	}
	t.Cleanup(c.Close)
	up := func() bool { ok, _, _ := c.Link(); return ok }
	waitFor(t, 10*time.Second, up)

	b.frozen.Store(true)
	frozenAt := time.Now()
	bound := time.Duration(MaxPingsOut+1)*PingInterval + 4*time.Second
	took := waitFor(t, bound, func() bool { return !up() })
	t.Logf("silent partition seen after %v (bound %v)", took, bound)
	if _, since, why := c.Link(); since.Before(frozenAt) || why == "" {
		t.Fatalf("the link's down state: since %v (frozen %v), reason %q", since, frozenAt, why)
	}

	b.frozen.Store(false)
	waitFor(t, 20*time.Second, up)
}
