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
