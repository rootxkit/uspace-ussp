package cis

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

type fakeKV struct {
	mu      sync.Mutex
	vals    map[string][]byte
	ops     []string
	failPut string
	failAll error
}

func newFakeKV() *fakeKV { return &fakeKV{vals: map[string][]byte{}} }

func (f *fakeKV) Put(_ context.Context, bucket, key string, v []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil || key == f.failPut {
		return errors.New("kv: unavailable")
	}
	f.vals[bucket+"/"+key] = v
	f.ops = append(f.ops, "put "+key)
	return nil
}

func (f *fakeKV) Delete(_ context.Context, bucket, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return f.failAll
	}
	delete(f.vals, bucket+"/"+key)
	f.ops = append(f.ops, "delete "+key)
	return nil
}

func (f *fakeKV) Keys(_ context.Context, bucket string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return nil, f.failAll
	}
	var out []string
	for k := range f.vals {
		if len(k) > len(bucket) && k[:len(bucket)+1] == bucket+"/" {
			out = append(out, k[len(bucket)+1:])
		}
	}
	slices.Sort(out)
	return out, nil
}

func projection(cells ...string) *Projection {
	p := &Projection{Basis: Basis{CISVersion: "zones:1"}, At: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC), Cells: map[string]CellEntry{}}
	for _, c := range cells {
		p.Cells[c] = CellEntry{Cell: c, CISVersion: "zones:1", Zones: []ApplicableZone{{Identifier: "TZP001"}}}
	}
	return p
}

// Cells are written under their KV form, the basis last; a cell no
// longer listed is deleted (also one left by a previous process, read
// from the bucket on the first projection).
func TestBusProjectorWritesCellsThenBasis(t *testing.T) {
	kv := newFakeKV()
	kv.vals[BucketCISCurrent+"/c5.1.1"] = []byte(`{}`) // left by an earlier process
	b := &BusProjector{KV: kv}
	if err := b.ProjectCIS(context.Background(), projection("c5:1317:2248", AllCells)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kv.ops, []string{"put all", "put c5.1317.2248", "delete c5.1.1", "put basis"}) {
		t.Fatalf("ops %v", kv.ops)
	}
	var basis BasisValue
	if err := json.Unmarshal(kv.vals[BucketCISCurrent+"/"+KeyBasis], &basis); err != nil || basis.CISVersion != "zones:1" || basis.Cells != 2 {
		t.Fatalf("basis %+v %v", basis, err)
	}
	var e CellEntry
	if err := json.Unmarshal(kv.vals[BucketCISCurrent+"/c5.1317.2248"], &e); err != nil || e.Cell != "c5:1317:2248" || len(e.Zones) != 1 {
		t.Fatalf("entry %+v %v", e, err)
	}
	kv.ops = nil
	if err := b.ProjectCIS(context.Background(), projection("c5:1317:2249")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kv.ops, []string{"put c5.1317.2249", "delete all", "delete c5.1317.2248", "put basis"}) {
		t.Fatalf("second ops %v", kv.ops)
	}
}

// A write that fails stops the projection before the basis moves, and
// the next projection still deletes what the failed one did not.
func TestBusProjectorFailureKeepsTheBasis(t *testing.T) {
	kv := newFakeKV()
	b := &BusProjector{KV: kv}
	if err := b.ProjectCIS(context.Background(), projection("c5:1:1", "c5:1:2")); err != nil {
		t.Fatal(err)
	}
	kv.failPut = "c5.2.2"
	if err := b.ProjectCIS(context.Background(), projection("c5:2:2")); err == nil {
		t.Fatal("failed put not returned")
	}
	var basis BasisValue
	_ = json.Unmarshal(kv.vals[BucketCISCurrent+"/"+KeyBasis], &basis)
	if basis.Cells != 2 {
		t.Fatalf("the basis moved on a failed projection: %+v", basis)
	}
	kv.failPut = ""
	kv.ops = nil
	if err := b.ProjectCIS(context.Background(), projection("c5:2:2")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kv.ops, []string{"put c5.2.2", "delete c5.1.1", "delete c5.1.2", "put basis"}) {
		t.Fatalf("ops %v", kv.ops)
	}
	kv.failAll = errors.New("down")
	if err := (&BusProjector{KV: kv}).ProjectCIS(context.Background(), projection()); err == nil {
		t.Fatal("unreadable bucket not returned")
	}
}
