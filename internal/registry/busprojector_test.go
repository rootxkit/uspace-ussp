package registry

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

type recKV struct {
	vals map[string][]byte
	ops  []string
	err  error
}

func (r *recKV) Put(_ context.Context, bucket, key string, v []byte) error {
	if r.err != nil {
		return r.err
	}
	r.vals[key] = v
	r.ops = append(r.ops, bucket+" put "+key)
	return nil
}

func (r *recKV) Delete(_ context.Context, bucket, key string) error {
	if r.err != nil {
		return r.err
	}
	delete(r.vals, key)
	r.ops = append(r.ops, bucket+" delete "+key)
	return nil
}

func (r *recKV) Keys(_ context.Context, _ string) ([]string, error) {
	if r.err != nil {
		return nil, r.err
	}
	out := make([]string, 0, len(r.vals))
	for k := range r.vals {
		out = append(out, k)
	}
	slices.Sort(out)
	return out, nil
}

// DeleteFolds deletes every key of the bucket an invalidation's fold
// names (the serial in any case and punctuation), and nothing else: not
// another serial, not an operator with the same text.
func TestBusProjectorDeleteFolds(t *testing.T) {
	kv := &recKV{vals: map[string][]byte{}}
	b := BusProjector{KV: kv}
	for _, k := range []Key{{EntityUAS, "test-sn-a"}, {EntityUAS, "TEST-SN-A"}, {EntityUAS, "TEST-SN-B"}, {EntityOperator, "TEST-SN-A"}} {
		kv.vals[KVKey(k)] = []byte(`{}`)
	}
	kv.vals["not-a-registry-key"] = []byte(`{}`)
	got, err := b.DeleteFolds(context.Background(), []Invalidation{{Entity: EntityUAS, KeyFold: entryKeyFold(EntityUAS, "TEST-SN-A")}})
	if err != nil || len(got) != 2 {
		t.Fatalf("deleted %v %v", got, err)
	}
	for _, k := range []Key{{EntityUAS, "TEST-SN-B"}, {EntityOperator, "TEST-SN-A"}} {
		if _, ok := kv.vals[KVKey(k)]; !ok {
			t.Errorf("%v deleted", k)
		}
	}
	if _, ok := kv.vals["not-a-registry-key"]; !ok {
		t.Error("a foreign key deleted")
	}
	kv.err = errors.New("kv down")
	if _, err := b.DeleteFolds(context.Background(), []Invalidation{{Entity: EntityUAS, KeyFold: "X"}}); err == nil {
		t.Fatal("a listing failure not returned")
	}
}

func TestBusProjector(t *testing.T) {
	kv := &recKV{vals: map[string][]byte{}}
	b := BusProjector{KV: kv}
	e := Entry{Key: Key{Entity: "uas", Key: "TEST-SERIAL 1/A"}, KeyFold: "TESTSERIAL1A", Status: "valid", FetchedAt: time.Now().UTC()}
	if err := b.ProjectRegistry(context.Background(), []Entry{e}, []Key{{Entity: "operator", Key: "GEO-TEST-OP-1"}}); err != nil {
		t.Fatal(err)
	}
	k := KVKey(e.Key)
	if !bus.ValidKey(k) || !slices.Equal(kv.ops, []string{"registry_validity delete operator." + bus.KeyToken("GEO-TEST-OP-1"), "registry_validity put " + k}) {
		t.Fatalf("ops %v key %q", kv.ops, k)
	}
	var back Entry
	if err := json.Unmarshal(kv.vals[k], &back); err != nil || back.Key != e.Key || back.Status != "valid" {
		t.Fatalf("%+v %v", back, err)
	}
	kv.err = errors.New("kv down")
	if err := b.ProjectRegistry(context.Background(), []Entry{e}, nil); err == nil {
		t.Fatal("put failure not returned")
	}
	if err := b.ProjectRegistry(context.Background(), nil, []Key{e.Key}); err == nil {
		t.Fatal("delete failure not returned")
	}
}
