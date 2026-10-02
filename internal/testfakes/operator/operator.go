// Package operator is a simulated operator client of this USSP (brief
// WP-8; also used by uspace-lab): it streams telemetry/v1 samples of its
// aircraft over WS /v1/telemetry with a bearer token, keeps every sample
// until the status frames acknowledge it (acked_seq, B-05), queues what
// it cannot send while it is Down (an outage: the socket closed) for at
// most ten minutes, and drains the queue through POST
// /v1/telemetry/batch with sent_at and backlog true, at most one second
// of samples per serial per batch (spec 02 F5). The wire shapes are the
// generated client's (internal/national/client, from api/openapi.yaml),
// never written here by hand. Tests and the lab only: it has no send path
// to an aircraft, only to the USSP.
package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/rootxkit/uspace-ussp/internal/national/client"
)

// MaxQueue is the samples the client keeps unacknowledged: ten minutes
// of 100 aircraft at 1 Hz (spec 02 F5: the 10 min client queue).
const MaxQueue = 60_000

// sample is one sample with what the client knows of it.
type sample struct {
	frame client.TelemetryFrame
	// onSocket is the socket generation it was sent on (0: never sent).
	onSocket int
}

// Client is one simulated operator client.
type Client struct {
	// Base is the USSP's base URL (http://host:port).
	Base  string
	Token string
	// Serials are the aircraft it flies, all at Lat, Lng.
	Serials  []string
	Lat, Lng float64
	// IntentID, when set, is put on every sample.
	IntentID *string

	mu      sync.Mutex
	seq     map[string]int64
	pending map[string]map[int64]*sample // serial -> seq -> sample not acknowledged
	conn    *websocket.Conn
	gen     int
	cancel  context.CancelFunc
	reader  sync.WaitGroup
	dropped int // samples the queue bound let go
	statusN int
}

// New is a client of base with token for serials at lat, lng.
func New(base, token string, lat, lng float64, serials ...string) *Client {
	return &Client{Base: strings.TrimSuffix(base, "/"), Token: token, Serials: serials, Lat: lat, Lng: lng,
		seq: map[string]int64{}, pending: map[string]map[int64]*sample{}}
}

// Up opens the socket; the reader takes the status frames and lets the
// acknowledged samples go.
func (c *Client) Up(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(c.Base, "http") + "/v1/telemetry"
	conn, resp, err := websocket.Dial(dctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + c.Token}}})
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial: %d: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("dial: %w", err)
	}
	conn.SetReadLimit(1 << 20)
	rctx, rcancel := context.WithCancel(context.WithoutCancel(ctx))
	c.mu.Lock()
	c.conn, c.cancel = conn, rcancel
	c.gen++
	gen := c.gen
	c.mu.Unlock()
	c.reader.Go(func() { c.read(rctx, conn, gen) })
	return nil
}

// Down closes the socket: an outage. Samples taken while down are queued.
func (c *Client) Down() {
	c.mu.Lock()
	conn, cancel := c.conn, c.cancel
	c.conn, c.cancel = nil, nil
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "outage")
		cancel()
		c.reader.Wait()
	}
}

// status is what the client reads of a console/status/v1 frame.
type status struct {
	Schema string `json:"schema"`
	Body   struct {
		AckedSeq map[string]int64 `json:"acked_seq"`
	} `json:"body"`
}

func (c *Client) read(ctx context.Context, conn *websocket.Conn, gen int) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var st status
		if json.Unmarshal(data, &st) != nil || st.Schema != "console/status/v1" {
			continue
		}
		c.mu.Lock()
		c.statusN++
		for sn, acked := range st.Body.AckedSeq {
			for seq, s := range c.pending[sn] {
				// acked_seq covers the samples sent on this socket only.
				if seq <= acked && s.onSocket == gen {
					delete(c.pending[sn], seq)
				}
			}
		}
		c.mu.Unlock()
	}
}

// Tick takes one sample of every aircraft at ts and sends it when the
// socket is up; it returns how many it sent.
func (c *Client) Tick(ctx context.Context, ts time.Time) (int, error) {
	c.mu.Lock()
	conn, gen := c.conn, c.gen
	var out []*sample
	for _, sn := range c.Serials {
		c.seq[sn]++
		f := c.frame(sn, c.seq[sn], ts)
		s := &sample{frame: f}
		if c.pending[sn] == nil {
			c.pending[sn] = map[int64]*sample{}
		}
		if c.queued() >= MaxQueue {
			c.dropOldest()
		}
		c.pending[sn][f.Seq] = s
		if conn != nil {
			s.onSocket = gen
			out = append(out, s)
		}
	}
	c.mu.Unlock()
	for _, s := range out {
		data, err := json.Marshal(map[string]any{"schema": "telemetry/v1", "body": s.frame})
		if err != nil {
			return 0, err
		}
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			return 0, err
		}
	}
	return len(out), nil
}

func (c *Client) queued() int {
	n := 0
	for _, m := range c.pending {
		n += len(m)
	}
	return n
}

// dropOldest lets the oldest sample go (the queue's bound), counted.
func (c *Client) dropOldest() {
	var sn string
	var seq int64 = -1
	var at time.Time
	for s, m := range c.pending {
		for q, smp := range m {
			if seq < 0 || smp.frame.Ts.Before(at) {
				sn, seq, at = s, q, smp.frame.Ts
			}
		}
	}
	if seq >= 0 {
		delete(c.pending[sn], seq)
		c.dropped++
	}
}

func (c *Client) frame(sn string, seq int64, ts time.Time) client.TelemetryFrame {
	alt, h, v, speed, track, acc := 650.0, 80.0, 0.0, 8.0, 90.0, 0.1
	ref := client.TelemetryFrameHeightRef("TakeoffLocation")
	f := client.TelemetryFrame{
		Ts: ts.UTC(), Serial: sn, Seq: seq, Position: client.TelemetryPosition{Lat: c.Lat, Lng: c.Lng},
		AltWgs84M: &alt, HeightM: &h, HeightRef: &ref, SpeedMs: &speed, TrackDeg: &track, VspeedMs: &v,
		Status: "Airborne", AccuracyH: "HA3m", AccuracyV: "VA10m", TimestampAccuracyS: &acc,
	}
	if c.IntentID != nil {
		// The generated UUID type reads its JSON string form.
		if b, err := json.Marshal(*c.IntentID); err == nil {
			var id client.TelemetryFrame
			if json.Unmarshal([]byte(`{"intent_id":`+string(b)+`}`), &id) == nil {
				f.IntentId = id.IntentId
			}
		}
	}
	return f
}

// Pending is the samples not acknowledged yet, and how many the queue
// bound let go.
func (c *Client) Pending() (pending, dropped int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queued(), c.dropped
}

// Statuses is the status frames read so far.
func (c *Client) Statuses() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusN
}

// DrainStats is what a drain measured.
type DrainStats struct {
	Samples  int
	Batches  int
	Accepted int
	Duration time.Duration
	// RatePerS is samples handed per second of the drain.
	RatePerS float64
	Refused  map[string]int
}

// Drain sends every queued sample that was never sent on a socket (or
// whose socket is gone) through POST /v1/telemetry/batch: backlog true,
// sent_at the client's clock, at most one second of samples per serial
// per batch, oldest first. A sample the answer settles (accepted,
// duplicate or refused) is let go; one not acknowledged is kept.
func (c *Client) Drain(ctx context.Context, hc *http.Client) (DrainStats, error) {
	api, err := client.NewClientWithResponses(c.Base, client.WithHTTPClient(hc))
	if err != nil {
		return DrainStats{}, err
	}
	c.mu.Lock()
	gen, up := c.gen, c.conn != nil
	var queue []*sample
	for _, m := range c.pending {
		for _, s := range m {
			if s.onSocket == 0 || !up || s.onSocket != gen {
				queue = append(queue, s)
			}
		}
	}
	c.mu.Unlock()
	st := DrainStats{Refused: map[string]int{}}
	start := time.Now()
	for len(queue) > 0 {
		sort.Slice(queue, func(i, j int) bool { return queue[i].frame.Ts.Before(queue[j].frame.Ts) })
		batch, rest := oneSecond(queue)
		queue = rest
		frames := make([]client.TelemetryFrame, len(batch))
		for i, s := range batch {
			f := s.frame
			yes := true
			f.Backlog = &yes
			frames[i] = f
		}
		sent := time.Now().UTC()
		resp, err := api.PostTelemetryBatchWithResponse(ctx, client.TelemetryBatch{SentAt: &sent, Frames: frames},
			func(_ context.Context, r *http.Request) error {
				r.Header.Set("Authorization", "Bearer "+c.Token)
				return nil
			})
		if err != nil {
			return st, err
		}
		if resp.JSON202 == nil {
			return st, fmt.Errorf("batch: %d %s", resp.StatusCode(), resp.Body)
		}
		st.Batches++
		st.Samples += len(batch)
		st.Accepted += resp.JSON202.Accepted
		kept := map[int]bool{}
		for _, o := range resp.JSON202.Outcomes {
			switch o.Outcome {
			case "not_acknowledged", "duplicate_pending", "dropped_queue_full", "dropped_rate", "refused_batch_span":
				kept[o.Index] = true
			case "duplicate":
			default:
				st.Refused[o.Outcome]++
			}
		}
		c.mu.Lock()
		for i, s := range batch {
			if !kept[i] {
				delete(c.pending[s.frame.Serial], s.frame.Seq)
			} else {
				queue = append(queue, s) // sent again in a later batch
			}
		}
		c.mu.Unlock()
		if len(kept) > 0 {
			// Something must be sent again (the backlog rate bound): wait
			// a little rather than spin.
			t := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				t.Stop()
				return st, ctx.Err()
			case <-t.C:
			}
		}
	}
	st.Duration = time.Since(start)
	if st.Duration > 0 {
		st.RatePerS = float64(st.Accepted) / st.Duration.Seconds()
	}
	return st, nil
}

// oneSecond cuts from queue (sorted by time) the samples within one
// second of the oldest of each serial.
func oneSecond(queue []*sample) (batch, rest []*sample) {
	first := map[string]time.Time{}
	for _, s := range queue {
		f0, ok := first[s.frame.Serial]
		if !ok {
			first[s.frame.Serial] = s.frame.Ts
			f0 = s.frame.Ts
		}
		if s.frame.Ts.Sub(f0) <= time.Second && len(batch) < 2000 {
			batch = append(batch, s)
		} else {
			rest = append(rest, s)
		}
	}
	return batch, rest
}

// ErrNotDrained is returned by WaitAcked when samples stay unacknowledged.
var ErrNotDrained = errors.New("samples not acknowledged")

// WaitAcked waits until every sample is acknowledged.
func (c *Client) WaitAcked(ctx context.Context, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		if p, _ := c.Pending(); p == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			p, _ := c.Pending()
			return fmt.Errorf("%w: %d left", ErrNotDrained, p)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
