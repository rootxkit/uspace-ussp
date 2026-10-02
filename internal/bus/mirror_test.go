package bus

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// entry is a jetstream.KeyValueEntry of a test.
type entry struct {
	key string
	val []byte
	op  jetstream.KeyValueOp
}

func (e entry) Bucket() string                  { return "b" }
func (e entry) Key() string                     { return e.key }
func (e entry) Value() []byte                   { return e.val }
func (e entry) Revision() uint64                { return 1 }
func (e entry) Created() time.Time              { return time.Time{} }
func (e entry) Delta() uint64                   { return 0 }
func (e entry) Operation() jetstream.KeyValueOp { return e.op }

func put(k, v string) jetstream.KeyValueEntry {
	return entry{key: k, val: []byte(v), op: jetstream.KeyValuePut}
}
func del(k string) jetstream.KeyValueEntry { return entry{key: k, op: jetstream.KeyValueDelete} }

// Before the first marker the mirror says "not read" (SC-22); after it,
// an empty bucket is "read, nothing there" (E-01 pair).
func TestMirrorTellsNotReadFromEmpty(t *testing.T) {
	m := &Mirror[int]{Bucket: "b"}
	if _, _, ok := m.Snapshot(); ok {
		t.Fatal("loaded before the watch delivered anything")
	}
	if _, found, _, loaded := m.Get("a"); found || loaded {
		t.Fatal("a key before the bucket was read")
	}
	m.building = map[string]int{}
	m.take(nil)
	vals, age, ok := m.Snapshot()
	if !ok || len(vals) != 0 || age != 0 {
		t.Fatalf("empty bucket: %v %v %v", vals, age, ok)
	}
}

func TestMirrorAppliesPutsDeletesAndKeepsTheLastReadable(t *testing.T) {
	now := time.Unix(1000, 0)
	var changes []string
	m := &Mirror[int]{Bucket: "b", MaxKeys: 2, Now: func() time.Time { return now }, OnChange: func(k string) { changes = append(changes, k) }}
	m.building = map[string]int{}
	m.take(put("a", "1"))
	if _, _, ok := m.Snapshot(); ok {
		t.Fatal("initial values served before the marker")
	}
	m.take(nil)
	m.take(put("b", "2"))
	m.take(put("c", "3")) // over the bound: counted, not kept
	m.take(put("a", "x")) // does not decode: the last value is kept
	m.take(del("b"))
	vals, _, _ := m.Snapshot()
	if len(vals) != 1 || vals["a"] != 1 {
		t.Fatalf("values %v", vals)
	}
	c := m.counters()
	if c.Get(CounterMirrorOverBound) != 1 || c.Get(CounterMirrorDecodeFailed) != 1 || c.Get(CounterMirrorDeleted) != 1 || c.Get(CounterMirrorApplied) != 2 {
		t.Fatalf("counters %v", c.Snapshot())
	}
	if strings.Join(changes, ",") != ",b,b" {
		t.Fatalf("changes %q", changes)
	}
	// A broken watch serves the last values with their age.
	m.live = false
	now = now.Add(7 * time.Second)
	if _, found, age, loaded := m.Get("a"); !found || !loaded || age != 7 {
		t.Fatalf("after the watch ended: %v %v %v", found, age, loaded)
	}
	// A new watch replaces the values whole at its marker.
	m.building = map[string]int{}
	m.take(put("z", "9"))
	if _, found, _, _ := m.Get("z"); found {
		t.Fatal("a new watch's value served before its marker")
	}
	m.take(nil)
	vals, age, _ := m.Snapshot()
	if len(vals) != 1 || vals["z"] != 9 || age != 0 {
		t.Fatalf("after the new marker: %v %v", vals, age)
	}
	if m.Len() != 1 {
		t.Fatal(m.Len())
	}
}

func TestMirrorDecoder(t *testing.T) {
	m := &Mirror[string]{Bucket: "b", Decode: func(k string, d []byte) (string, error) {
		if k == "bad" {
			return "", errors.New("no")
		}
		return k + "=" + string(d), nil
	}}
	m.building = map[string]string{}
	m.take(put("k", "v"))
	m.take(put("bad", "v"))
	m.take(nil)
	if v, found, _, _ := m.Get("k"); !found || v != "k=v" {
		t.Fatalf("%q %v", v, found)
	}
	if _, found, _, _ := m.Get("bad"); found {
		t.Fatal("a refused value kept")
	}
}

// E-01 pair: sequences removed above the consumer's delivered floor are
// a loss; a stream whose first sequence follows the floor has none.
func TestNeverDelivered(t *testing.T) {
	if from, to, _ := neverDelivered(10, 11); from <= to {
		t.Errorf("caught up: %d..%d", from, to)
	}
	if from, to, _ := neverDelivered(10, 5); from <= to {
		t.Errorf("first behind delivered: %d..%d", from, to)
	}
	if from, to, _ := neverDelivered(10, 15); from != 11 || to != 14 {
		t.Errorf("aged out: %d..%d", from, to)
	}
	if from, to, _ := neverDelivered(0, 3); from != 1 || to != 2 {
		t.Errorf("never read: %d..%d", from, to)
	}
}

// Seed is a complete read: loaded, current, the values a copy.
func TestMirrorSeed(t *testing.T) {
	changed := 0
	m := &Mirror[int]{Bucket: "b", OnChange: func(string) { changed++ }}
	in := map[string]int{"a": 1}
	m.Seed(in)
	in["a"] = 2
	if v, found, age, loaded := m.Get("a"); !found || !loaded || age != 0 || v != 1 || changed != 1 {
		t.Fatalf("%v %v %v %v %d", v, found, age, loaded, changed)
	}
}
