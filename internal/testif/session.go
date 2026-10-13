package testif

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/coder/websocket"

	"github.com/rootxkit/uspace-ussp/internal/national/client"
)

// The schemas of the telemetry socket's messages (api/openapi.yaml,
// openTelemetryStream: {"schema": "telemetry/v1", "body": TelemetryFrame}
// from the client, console/status/v1 from the server).
const (
	schemaTelemetry = "telemetry/v1"
	schemaStatus    = "console/status/v1"
)

// maxStatusBytes bounds one message the ingest sends.
const maxStatusBytes = 64 << 10

// outgoing is one message to the ingest.
type outgoing struct {
	Schema string                `json:"schema"`
	Body   client.TelemetryFrame `json:"body"`
}

// ingestStatus is what the injection reads of a console/status/v1 frame
// of the telemetry socket: the extras api/openapi.yaml names for it
// (accepted, refused, dropped, outcomes; TestStatusMembersAreTheIngests
// holds the names to telemetry-ingest's).
type ingestStatus struct {
	Accepted uint64            `json:"accepted"`
	Refused  uint64            `json:"refused"`
	Dropped  uint64            `json:"dropped"`
	Outcomes map[string]uint64 `json:"outcomes"`
}

// decided is how many samples the ingest has decided on the socket.
func (s ingestStatus) decided() uint64 {
	return s.Accepted + s.Refused + s.Dropped + s.Outcomes["duplicate"] + s.Outcomes["duplicate_pending"]
}

// refusals are the outcome names other than acceptance whose count grew
// since before, sorted.
func (s ingestStatus) refusals(before ingestStatus) []string {
	var out []string
	for k, v := range s.Outcomes {
		if k == "accepted" || k == "duplicate" || k == "duplicate_pending" {
			continue
		}
		if v > before.Outcomes[k] {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

type incoming struct {
	Schema string       `json:"schema"`
	Body   ingestStatus `json:"body"`
}

// RefusalError is the USSP's refusal on the real path: the problem type (or
// the ingest's outcome names) and its detail. The injection or the plan
// fails with it, never retries past it.
type RefusalError struct {
	Status int
	Reason string
	Detail string
}

func (r *RefusalError) Error() string {
	if r.Detail == "" {
		return r.Reason
	}
	return r.Reason + ": " + r.Detail
}

// problemOf reads a problem body (bounded) into a RefusalError.
func problemOf(status int, body []byte) *RefusalError {
	var p struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
		Errors []struct {
			Field  string `json:"field"`
			Reason string `json:"reason"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(body, &p)
	slug := p.Type
	if i := strings.LastIndex(slug, "/"); i >= 0 {
		slug = slug[i+1:]
	}
	if slug == "" {
		slug = fmt.Sprintf("http_%d", status)
	}
	detail := p.Detail
	for _, e := range p.Errors {
		detail += fmt.Sprintf("; %s: %s", e.Field, e.Reason)
	}
	return &RefusalError{Status: status, Reason: clip(slug, 64), Detail: clip(strings.TrimPrefix(detail, "; "), 512)}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// session is one WS /v1/telemetry socket of the test operator's client:
// one injected flight's (one aircraft is streamed by one socket). A
// reader takes every status frame; waiters wait for the ingest to decide
// on what they sent.
type session struct {
	conn *websocket.Conn

	wmu  sync.Mutex // one writer at a time; never held with mu
	sent uint64     // frames written on the socket (under wmu)

	mu      sync.Mutex
	last    ingestStatus
	changed chan struct{}
	err     error
	done    chan struct{}
}

// dial opens a socket with token. A refusal before the upgrade (no
// token, the operator_ws source switched off, ...) is a *RefusalError with the
// problem the USSP answered.
func dial(ctx context.Context, u *USSP, token string) (*session, error) {
	url := "ws" + strings.TrimPrefix(u.Base, "http") + "/v1/telemetry"
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: u.HTTP,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			var b []byte
			if resp.Body != nil {
				b, _ = io.ReadAll(io.LimitReader(resp.Body, maxStatusBytes))
				_ = resp.Body.Close()
			}
			return nil, problemOf(resp.StatusCode, b)
		}
		return nil, fmt.Errorf("WS /v1/telemetry: the USSP's telemetry-ingest did not open the socket: %w", err)
	}
	conn.SetReadLimit(maxStatusBytes)
	s := &session{conn: conn, changed: make(chan struct{}), done: make(chan struct{})}
	go s.read() //nolint:gosec // G118: the socket outlives the request that opened it; close ends the read
	return s, nil
}

// read takes every status frame until the socket ends.
func (s *session) read() {
	defer close(s.done)
	for {
		_, b, err := s.conn.Read(context.Background())
		if err != nil {
			s.mu.Lock()
			s.err = err
			close(s.changed)
			s.mu.Unlock()
			return
		}
		var in incoming
		if json.Unmarshal(b, &in) != nil || in.Schema != schemaStatus {
			continue // the socket carries only status frames; anything else is ignored
		}
		s.mu.Lock()
		s.last = in.Body
		close(s.changed)
		s.changed = make(chan struct{})
		s.mu.Unlock()
	}
}

// status is the latest status and the channel closed at the next one.
func (s *session) status() (ingestStatus, chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last, s.changed, s.err
}

// send writes one frame and returns how many the socket has carried.
func (s *session) send(ctx context.Context, f client.TelemetryFrame) (uint64, error) {
	b, err := json.Marshal(outgoing{Schema: schemaTelemetry, Body: f})
	if err != nil {
		return 0, err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if err := s.conn.Write(ctx, websocket.MessageText, b); err != nil {
		return 0, err
	}
	s.sent++
	return s.sent, nil
}

// errNotDecided is a frame the ingest did not decide on in time.
var errNotDecided = errors.New("telemetry-ingest did not acknowledge the frame in time")

// sendAndWait sends f and waits until the ingest has decided on every
// frame the socket carried up to it: nil when none of them since the
// call was refused, a *RefusalError naming the outcomes otherwise. ctx bounds
// the wait.
func (s *session) sendAndWait(ctx context.Context, f client.TelemetryFrame) error {
	before, _, err := s.status()
	if err != nil {
		return fmt.Errorf("the telemetry socket is closed: %w", err)
	}
	n, err := s.send(ctx, f)
	if err != nil {
		return fmt.Errorf("the frame could not be sent: %w", err)
	}
	for {
		st, changed, err := s.status()
		if st.decided() >= n {
			if refused := st.refusals(before); len(refused) > 0 {
				return &RefusalError{Status: http.StatusBadRequest, Reason: strings.Join(refused, ","),
					Detail: "telemetry-ingest refused the frame of " + f.Serial}
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("the telemetry socket closed before the frame was acknowledged: %w", err)
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return errNotDecided
		}
	}
}

// close ends the socket normally and waits for the reader.
func (s *session) close() {
	_ = s.conn.Close(websocket.StatusNormalClosure, "")
	<-s.done
}
