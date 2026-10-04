package records

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// newDaily is a Daily over the fixture with flights on day t0 (three)
// and the next day (one), at now.
func newDaily(t *testing.T, now time.Time) (*Daily, *memReader) {
	t.Helper()
	m, s := fixture()
	m.now = now
	base := m.flights[flightID]
	for i, at := range []time.Time{t0.Add(time.Hour), t0.Add(2 * time.Hour), t0.Add(24 * time.Hour)} {
		f := base
		f.ID = fmt.Sprintf("0b5d4c3a-2e1f-4a0b-9c8d-7e6f5a4b3c%02d", i)
		f.StartedAt = at
		m.flights[f.ID] = f
	}
	return &Daily{Builder: builder(m, s), Store: m, Dir: t.TempDir(), Counters: &core.Counters{}}, m
}

func readBundle(t *testing.T, path string) []Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var out []Record
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// The done-when bundle: built for the day, every flight of the day one
// record per line, the hash recorded is the file's, Open serves it, and
// a byte changed in the file is refused (E-01 pair).
func TestDailyBundle(t *testing.T) {
	d, m := newDaily(t, t0.Add(26*time.Hour))
	b, ok, err := d.Build(context.Background(), t0)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	path := filepath.Join(d.Dir, b.Ref)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != b.Hash || b.Flights != 3 || m.bundles[t0.Format(time.DateOnly)].Hash != b.Hash {
		t.Fatalf("bundle %+v", b)
	}
	recs := readBundle(t, path)
	if len(recs) != 3 || recs[0].Schema != Schema || !recs[0].Flight.StartedAt.Before(recs[1].Flight.StartedAt) {
		t.Fatalf("records %d", len(recs))
	}
	got, f, err := d.Open(context.Background(), t0)
	if err != nil || got.Hash != b.Hash {
		t.Fatalf("open: %v", err)
	}
	_ = f.Close()
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Open(context.Background(), t0); !errors.Is(err, ErrBundleCorrupt) {
		t.Fatalf("a changed bundle was served: %v", err)
	}
	if _, _, err := d.Open(context.Background(), t0.AddDate(0, 0, 3)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a day without a bundle: %v", err)
	}
	// A second build of the same day loses and leaves the stored file in
	// place (the same bytes have the same name); one with other bytes
	// leaves no file behind.
	if err := os.WriteFile(path, func() []byte { raw[len(raw)-1] ^= 0xff; return raw }(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.Build(context.Background(), t0); ok || err != nil {
		t.Fatalf("second build: %v %v", ok, err)
	}
	if _, f, err := d.Open(context.Background(), t0); err != nil {
		t.Fatalf("the stored bundle was removed by a losing build: %v", err)
	} else {
		_ = f.Close()
	}
	m.now = m.now.Add(time.Second) // another generated_at: other bytes
	if _, ok, err := d.Build(context.Background(), t0); ok || err != nil {
		t.Fatalf("third build: %v %v", ok, err)
	}
	if es, _ := os.ReadDir(d.Dir); len(es) != 1 {
		t.Fatalf("files left: %d", len(es))
	}
}

// BuildDue builds a day only from 01:00 UTC the next day, and catches up
// the days it missed (E-01: before 01:00 nothing is built).
func TestBuildDue(t *testing.T) {
	day := Day(t0)
	d, m := newDaily(t, day.Add(24*time.Hour+59*time.Minute))
	n, err := d.BuildDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.bundles[t0.Format(time.DateOnly)]; ok {
		t.Fatal("built before 01:00 the next day")
	}
	if n != CatchUpDays {
		t.Fatalf("caught up %d earlier days", n)
	}
	m.now = day.Add(25 * time.Hour)
	if n, err := d.BuildDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("at 01:00: %d %v", n, err)
	}
	if b := m.bundles[t0.Format(time.DateOnly)]; b.Flights != 3 {
		t.Fatalf("the day's bundle %+v", b)
	}
	if n, _ := d.BuildDue(context.Background()); n != 0 {
		t.Fatalf("built again: %d", n)
	}
}

// The missing-day alarm (E-02): degraded naming the day from 02:00 UTC,
// up once it is built, degraded without a directory, unknown when the
// table cannot be read.
func TestDailyProbe(t *testing.T) {
	day := Day(t0)
	d, m := newDaily(t, day.Add(26*time.Hour+time.Minute))
	for i := 1; i <= CatchUpDays; i++ {
		m.bundles[t0.AddDate(0, 0, -i).Format(time.DateOnly)] = Bundle{}
	}
	state, detail := d.Probe()(context.Background())
	if state != obs.StateDegraded || detail != "records: day "+t0.Format(time.DateOnly)+" missing" {
		t.Fatalf("%s %s", state, detail)
	}
	m.now = day.Add(25*time.Hour + 30*time.Minute)
	if state, _ := d.Probe()(context.Background()); state != obs.StateUp {
		t.Fatalf("missing before 02:00: %s", state)
	}
	m.now = day.Add(26*time.Hour + time.Minute)
	if _, _, err := d.Build(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	if state, detail := d.Probe()(context.Background()); state != obs.StateUp || !strings.Contains(detail, t0.Format(time.DateOnly)) {
		t.Fatalf("%s %s", state, detail)
	}
	m.errOf["missing"] = errDown
	if state, _ := d.Probe()(context.Background()); state != obs.StateUnknown {
		t.Fatalf("unreadable: %s", state)
	}
	m.errOf["missing"], m.errOf["now"] = nil, errDown
	if state, _ := d.Probe()(context.Background()); state != obs.StateUnknown {
		t.Fatalf("no clock: %s", state)
	}
	// Without a directory: degraded while a flight of the window has no
	// bundle, up (said) when there is none (E-01 pair).
	m.errOf["now"] = nil
	d.Dir = ""
	if state, detail := d.Probe()(context.Background()); state != obs.StateDegraded || !strings.Contains(detail, "USSP_RECORDS_DIR") {
		t.Fatalf("%s %s", state, detail)
	}
	m.now = day.AddDate(0, 1, 0)
	if state, detail := d.Probe()(context.Background()); state != obs.StateUp || !strings.Contains(detail, "no flight to bundle") {
		t.Fatalf("no flight: %s %s", state, detail)
	}
	m.errOf["day"] = errDown
	if state, _ := d.Probe()(context.Background()); state != obs.StateUnknown {
		t.Fatalf("flights unreadable: %s", state)
	}
	m.errOf["day"] = nil
	if n, err := d.BuildDue(context.Background()); n != 0 || err != nil {
		t.Fatal("built without a directory")
	}
	if _, _, err := d.Build(context.Background(), t0); err == nil {
		t.Fatal("Build without a directory")
	}
	if _, _, err := d.Open(context.Background(), t0); err == nil {
		t.Fatal("Open without a directory")
	}
}

// E-10 and failures: a day over MaxFlights is refused whole, never
// thinned; a record that cannot be built, an unreadable day or a store
// that refuses the row leave no bundle and no file.
func TestBundleRefusals(t *testing.T) {
	ctx := context.Background()
	d, m := newDaily(t, t0.Add(26*time.Hour))
	d.MaxFlights = 2
	if _, _, err := d.Build(ctx, t0); err == nil || !strings.Contains(err.Error(), "refused whole") {
		t.Fatalf("over the bound: %v", err)
	}
	d.MaxFlights = 3
	m.errOf["now"] = errDown
	if _, _, err := d.Build(ctx, t0); err == nil {
		t.Fatal("a record without a clock")
	}
	m.errOf["now"], m.errOf["day"] = nil, errDown
	if _, _, err := d.Build(ctx, t0); err == nil {
		t.Fatal("unreadable day")
	}
	m.errOf["day"], m.errOf["insert"] = nil, errDown
	if _, _, err := d.Build(ctx, t0); err == nil {
		t.Fatal("refused row")
	}
	m.errOf["insert"], m.errOf["audit"] = nil, errDown
	if _, ok, err := d.Build(ctx, t0); err != nil || !ok {
		t.Fatalf("an events row that fails does not undo a built bundle: %v %v", ok, err)
	}
	es, _ := os.ReadDir(d.Dir)
	if len(es) != 1 || d.Counters.Get(CounterBundlesFailed) != 4 {
		t.Fatalf("files %d counters %v", len(es), d.Counters.Snapshot())
	}
	m.bundles[t0.Format(time.DateOnly)] = Bundle{Ref: "../etc/passwd"}
	if _, _, err := d.Open(ctx, t0); err == nil {
		t.Fatal("a storage_ref outside the directory was opened")
	}
	m.bundles[t0.Format(time.DateOnly)] = Bundle{Ref: "2026-10-03-000000000000.jsonl.gz"}
	if _, _, err := d.Open(ctx, t0); err == nil {
		t.Fatal("a missing file was served")
	}
	m.errOf["bundle"] = errDown
	if _, _, err := d.Open(ctx, t0); !errors.Is(err, errDown) {
		t.Fatal("bundle table down")
	}
	m.errOf["missing"] = errDown
	if _, err := d.BuildDue(ctx); err == nil {
		t.Fatal("BuildDue without the table")
	}
}

// Run builds and stops with its context.
func TestDailyRun(t *testing.T) {
	d, m := newDaily(t, t0.Add(26*time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx, 10*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		_, ok := m.bundles[t0.Format(time.DateOnly)]
		m.mu.Unlock()
		if ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if _, ok := m.bundles[t0.Format(time.DateOnly)]; !ok {
		t.Fatal("Run built nothing")
	}
}
