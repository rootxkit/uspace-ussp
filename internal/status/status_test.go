package status

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/authority"
)

const certID = "0123456789abcdef0123456789abcdef"

type memStore struct {
	mu      sync.Mutex
	now     time.Time
	notices []Notice // oldest first
	errOf   map[string]error
	// follows is the id each notice follows, by its id (the unique
	// index of 00023).
	follows map[string]string
	// beforeInsert, when set, runs once at the next Insert before it
	// stores anything (a request racing this one).
	beforeInsert func()
}

func newMem() *memStore {
	return &memStore{now: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC), errOf: map[string]error{}}
}

func (m *memStore) Notices(_ context.Context, cert string, n int) ([]Notice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOf["notices"]; err != nil {
		return nil, err
	}
	var out []Notice
	for i := len(m.notices) - 1; i >= 0 && len(out) < n; i-- {
		if m.notices[i].CertificateID == cert {
			out = append(out, m.notices[i])
		}
	}
	return out, nil
}

func (m *memStore) Insert(_ context.Context, kind, cert, ref, by, follows string) (Notice, error) {
	if f := m.beforeInsert; f != nil {
		m.beforeInsert = nil
		f()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOf["insert"]; err != nil {
		return Notice{}, err
	}
	for i := range m.notices {
		if n := &m.notices[i]; kind == KindStart && n.Kind == KindStart && n.CertificateID == cert && n.State != "failed" {
			return Notice{}, ErrStartExists
		}
		if n := &m.notices[i]; follows != "" && m.follows[n.ID] == follows && n.State != "failed" {
			return Notice{}, ErrFollowed
		}
	}
	next := m.now
	n := Notice{ID: fmt.Sprint(len(m.notices) + 1), Kind: kind, At: m.now, CertificateID: cert, Reference: ref, RequestedBy: by, State: "pending",
		NextAt: &next, CreatedAt: m.now}
	m.notices = append(m.notices, n)
	if m.follows == nil {
		m.follows = map[string]string{}
	}
	m.follows[n.ID] = follows
	return n, nil
}

func (m *memStore) Claim(_ context.Context, n int, lease time.Duration) ([]Queued, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.errOf["claim"]; err != nil {
		return nil, err
	}
	var out []Queued
	blocked := map[string]bool{}
	for i := range m.notices {
		x := &m.notices[i]
		if x.State != "pending" {
			continue
		}
		first := !blocked[x.CertificateID]
		blocked[x.CertificateID] = true
		if !first || x.NextAt.After(m.now) || len(out) >= n {
			continue
		}
		x.Attempts++
		t := m.now.Add(lease)
		x.NextAt = &t
		out = append(out, Queued{ID: x.ID, Kind: x.Kind, At: x.At, CertificateID: x.CertificateID, Reference: x.Reference, Attempts: x.Attempts})
	}
	return out, nil
}

func (m *memStore) find(id string) *Notice {
	for i := range m.notices {
		if m.notices[i].ID == id {
			return &m.notices[i]
		}
	}
	return nil
}

func (m *memStore) Delivered(_ context.Context, id, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if x := m.find(id); x != nil && x.State == "pending" {
		t := m.now
		x.State, x.AuthorityRef, x.SubmittedAt, x.NextAt, x.LastError = "delivered", &ref, &t, nil, nil
	}
	return m.errOf["delivered"]
}

func (m *memStore) Retry(_ context.Context, id, cause string, backoff time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if x := m.find(id); x != nil && x.State == "pending" {
		t := m.now.Add(backoff)
		x.NextAt, x.LastError = &t, &cause
	}
	return nil
}

func (m *memStore) Fail(_ context.Context, id, cause string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if x := m.find(id); x != nil && x.State == "pending" {
		t := m.now
		x.State, x.FailedAt, x.LastError, x.NextAt = "failed", &t, &cause, nil
	}
	return nil
}

func newService(st *memStore, a Authority) *Service {
	return &Service{Store: st, Authority: a, CertificateID: certID, SystemID: "DEV01", Counters: &core.Counters{}}
}

func fakeClient(t *testing.T) (*authority.Fake, *Client) {
	t.Helper()
	f := authority.New()
	t.Cleanup(f.Close)
	c, err := NewClient(f.URL(), authority.Tokens{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

// The done-when: the start is sent once and recorded; asked again (or
// after a restart of the process: a new Service on the same store) it
// answers the stored notice and sends nothing; cease and restart follow
// on request, each recorded by the authority with this USSP's reference.
func TestStartOnceCeaseAndRestart(t *testing.T) {
	fake, client := fakeClient(t)
	st := newMem()
	s := newService(st, client)
	ctx := context.Background()
	n, created, err := s.Request(ctx, "staff-1", KindStart)
	if err != nil || !created || n.State != "pending" {
		t.Fatalf("%+v %v %v", n, created, err)
	}
	if s.SendDue(ctx) != 1 || st.notices[0].State != "delivered" || st.notices[0].AuthorityRef == nil {
		t.Fatalf("start not recorded: %+v", st.notices[0])
	}
	restarted := newService(st, client)
	again, created, err := restarted.Request(ctx, "staff-2", KindStart)
	if err != nil || created || again.Reference != n.Reference || restarted.SendDue(ctx) != 0 {
		t.Fatalf("a second start: %+v %v %v", again, created, err)
	}
	for _, k := range []string{KindCease, KindRestart} {
		if _, created, err := s.Request(ctx, "staff-1", k); err != nil || !created {
			t.Fatalf("%s: %v %v", k, created, err)
		}
		if s.SendDue(ctx) != 1 {
			t.Fatalf("%s not recorded", k)
		}
	}
	got := fake.StatusNotices()
	if len(got) != 3 || got[0].State != "started" || got[1].State != "ceased" || got[2].State != "restarted" || got[0].Reference != n.Reference ||
		got[0].CertificateID != certID {
		t.Fatalf("at the authority: %+v", got)
	}
	if state, detail := s.Probe()(ctx); state != obs.StateUp || !strings.Contains(detail, "restart delivered") {
		t.Errorf("probe %s %s", state, detail)
	}
}

// The order of the notices (E-01 pairs): a cease needs a start, a
// restart needs a cease; asking again for the same state answers the
// last notice; an unknown kind and a missing certificate id are refused.
// Two requests for the same change that read the notices before either
// stored its own (two consoles, one click each): one notice is stored
// and both answer it; the chain stays start, cease, restart.
func TestConcurrentRequestsStoreOneNotice(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{KindCease, KindRestart} {
		st := newMem()
		s := newService(st, nil)
		if _, _, err := s.Request(ctx, "staff-1", KindStart); err != nil {
			t.Fatal(err)
		}
		if kind == KindRestart {
			if _, _, err := s.Request(ctx, "staff-1", KindCease); err != nil {
				t.Fatal(err)
			}
		}
		var other Notice
		var otherCreated bool
		var otherErr error
		st.beforeInsert = func() { other, otherCreated, otherErr = s.Request(ctx, "staff-2", kind) }
		mine, created, err := s.Request(ctx, "staff-1", kind)
		if err != nil || otherErr != nil {
			t.Fatalf("%s: %v %v", kind, err, otherErr)
		}
		n := 0
		for _, x := range st.notices {
			if x.Kind == kind {
				n++
			}
		}
		if n != 1 || created == otherCreated || mine.Reference != other.Reference {
			t.Fatalf("%s: %d stored; created %v and %v; references %s and %s", kind, n, created, otherCreated, mine.Reference, other.Reference)
		}
	}
}

func TestRequestRules(t *testing.T) {
	st := newMem()
	s := newService(st, nil)
	ctx := context.Background()
	var re *RequestError
	if _, _, err := s.Request(ctx, "a", KindCease); !errors.As(err, &re) {
		t.Fatalf("cease first: %v", err)
	}
	if _, _, err := s.Request(ctx, "a", KindRestart); !errors.As(err, &re) {
		t.Fatalf("restart first: %v", err)
	}
	if _, _, err := s.Request(ctx, "a", "pause"); !errors.As(err, &re) {
		t.Fatalf("unknown: %v", err)
	}
	if _, c, _ := s.Request(ctx, "a", KindStart); !c {
		t.Fatal("start")
	}
	if _, _, err := s.Request(ctx, "a", KindRestart); !errors.As(err, &re) {
		t.Fatalf("restart after a start: %v", err)
	}
	if _, c, _ := s.Request(ctx, "a", KindCease); !c {
		t.Fatal("cease")
	}
	if _, c, err := s.Request(ctx, "a", KindCease); c || err != nil {
		t.Fatalf("cease again: %v %v", c, err)
	}
	if n, c, err := s.Request(ctx, "a", KindStart); c || err != nil || n.Kind != KindStart {
		t.Fatalf("start after a cease answers the start: %+v %v %v", n, c, err)
	}
	none := newService(newMem(), nil)
	none.CertificateID = ""
	if _, _, err := none.Request(ctx, "a", KindStart); !errors.As(err, &re) || !strings.Contains(err.Error(), "USSP_CERTIFICATE_ID") {
		t.Fatalf("no certificate: %v", err)
	}
	if ns, err := none.List(ctx); err != nil || len(ns) != 0 {
		t.Fatal("list without a certificate")
	}
	if state, detail := none.Probe()(ctx); state != obs.StateUp || !strings.Contains(detail, "USSP_CERTIFICATE_ID is not set") {
		t.Errorf("probe without a certificate: %s %s", state, detail)
	}
	if state, detail := newService(newMem(), nil).Probe()(ctx); state != obs.StateDegraded || !strings.Contains(detail, "no start notice yet") {
		t.Errorf("probe before the start: %s %s", state, detail)
	}
	st.errOf["notices"] = errors.New("db")
	if _, _, err := s.Request(ctx, "a", KindStart); err == nil {
		t.Fatal("store down")
	}
	if state, _ := s.Probe()(ctx); state != obs.StateUnknown {
		t.Error("probe store down")
	}
}

// The authority down: retried with backoff and counted (E-02); a
// refusal (409) fails the notice for good; past MaxAttempts it gives up
// (E-10).
func TestDelivery(t *testing.T) {
	fake, client := fakeClient(t)
	st := newMem()
	s := newService(st, client)
	ctx := context.Background()
	_, _, _ = s.Request(ctx, "a", KindStart)
	fake.Down()
	for i := 1; i <= 2; i++ {
		if s.SendDue(ctx) != 0 || st.notices[0].Attempts != i || st.notices[0].LastError == nil {
			t.Fatalf("try %d: %+v", i, st.notices[0])
		}
		st.now = st.now.Add(Backoff(i))
	}
	if state, _ := s.Probe()(ctx); state != obs.StateDegraded {
		t.Error("probe while pending")
	}
	fake.Up()
	if s.SendDue(ctx) != 1 || s.Counters.Get(CounterRetried) != 2 {
		t.Fatalf("not recorded once back: %v", s.Counters.Snapshot())
	}

	// A cease the authority does not admit (a fresh authority holds no
	// start for the certificate).
	_, fresh := fakeClient(t)
	st2 := newMem()
	st2.notices = append(st2.notices, Notice{ID: "x", Kind: KindStart, CertificateID: certID, State: "delivered", Reference: "r0", CreatedAt: st2.now})
	s2 := newService(st2, fresh)
	_, _, _ = s2.Request(ctx, "a", KindCease)
	s2.SendDue(ctx)
	if n := st2.notices[1]; n.State != "failed" || !strings.Contains(*n.LastError, "409") {
		t.Fatalf("refused: %+v", n)
	}
	if state, _ := s2.Probe()(ctx); state != obs.StateDegraded {
		t.Error("probe after a failure")
	}

	st3 := newMem()
	s3 := newService(st3, client)
	s3.MaxAttempts = 2
	_, _, _ = s3.Request(ctx, "a", KindStart)
	fake.Down()
	for range 4 {
		s3.SendDue(ctx)
		st3.now = st3.now.Add(time.Hour)
	}
	if n := st3.notices[0]; n.State != "failed" || !strings.Contains(*n.LastError, "gave up after 2 tries") {
		t.Fatalf("bound: %+v", n)
	}
	fake.Up()
	nothing := newService(newMem(), nil)
	_, _, _ = nothing.Request(ctx, "a", KindStart)
	if nothing.SendDue(ctx) != 0 {
		t.Fatal("sent without an authority")
	}
	if state, detail := nothing.Probe()(ctx); state != obs.StateDegraded || !strings.Contains(detail, "no authority client") {
		t.Errorf("%s %s", state, detail)
	}
}

// The client against answers it must not take: a 200 that is not a
// result, a 5xx, a redirect, an oversized body, an unreachable host and
// a token source that fails; and the base URL rules.
func TestClientAnswers(t *testing.T) {
	var status int
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status == http.StatusFound {
			w.Header().Set("Location", "https://elsewhere.example/")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	c, err := NewClient(srv.URL, authority.Tokens{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	q := Queued{Kind: KindStart, At: time.Now(), CertificateID: certID, Reference: "r"}
	for _, tc := range []struct {
		status    int
		body      string
		permanent bool
	}{
		{200, `{}`, false}, {500, ``, false}, {http.StatusFound, ``, false}, {200, strings.Repeat(" ", MaxAnswerBytes+1), false},
		{403, `{"type":"https://schemas.uspace.ge/problems/forbidden"}`, true}, {404, `x`, true},
	} {
		status, body = tc.status, tc.body
		_, err := c.Post(context.Background(), q)
		var perm *PermanentError
		if err == nil || errors.As(err, &perm) != tc.permanent {
			t.Errorf("%d: %v", tc.status, err)
		}
	}
	srv.Close()
	if _, err := c.Post(context.Background(), q); err == nil || !strings.Contains(err.Error(), "not reached") {
		t.Errorf("unreachable: %v", err)
	}
	bad, _ := NewClient(srv.URL, failing{}, nil)
	if _, err := bad.Post(context.Background(), q); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Errorf("token: %v", err)
	}
	for _, u := range []string{"", "ftp://x", "https://u:p@x", "https://x/?a=b"} {
		if _, err := NewClient(u, authority.Tokens{}, nil); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	if _, err := NewClient("https://x", nil, nil); err == nil {
		t.Error("no tokens accepted")
	}
	if AuthorityState("pause") != "" {
		t.Error("AuthorityState")
	}
}

type failing struct{}

func (failing) Token(context.Context, string, ...string) (string, error) {
	return "", errors.New("down")
}

// Run sends and stops with its context.
func TestRun(t *testing.T) {
	_, client := fakeClient(t)
	st := newMem()
	s := newService(st, client)
	_, _, _ = s.Request(context.Background(), "a", KindStart)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, 5*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for s.Counters.Get(CounterDelivered) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if s.Counters.Get(CounterDelivered) != 1 {
		t.Fatal("Run did not send")
	}
	st.errOf["claim"] = errors.New("db")
	if s.SendDue(context.Background()) != 0 || s.Counters.Get(CounterStoreFailed) != 1 {
		t.Error("claim failure")
	}
}
