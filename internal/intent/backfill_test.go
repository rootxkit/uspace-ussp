package intent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/bus"
)

// activeKV is intent_active in memory: the values as stored, every
// write counted.
type activeKV struct {
	vals   map[string][]byte
	puts   int
	keyErr error
}

func (k *activeKV) Keys(_ context.Context, bucket string) ([]string, error) {
	if k.keyErr != nil {
		return nil, k.keyErr
	}
	var out []string
	for key := range k.vals {
		if b, k, ok := strings.Cut(key, "/"); ok && b == bucket {
			out = append(out, k)
		}
	}
	return out, nil
}

func (k *activeKV) Get(_ context.Context, bucket, key string) ([]byte, bool, error) {
	v, ok := k.vals[bucket+"/"+key]
	return v, ok, nil
}

func (k *activeKV) PutJSON(_ context.Context, bucket, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	k.vals[bucket+"/"+key] = b
	k.puts++
	return nil
}

// preDeploy is intent_active as the projection wrote it before
// intent/state/v1 carried the operator and the client: every intent the
// projector holds, without operator_id and client_id.
func preDeploy(t *testing.T, pr *memProjector) *activeKV {
	t.Helper()
	kv := &activeKV{vals: map[string][]byte{}}
	for id := range pr.kv {
		b := pr.kv[id]
		b.OperatorID, b.ClientID = "", ""
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		kv.vals[bus.BucketIntentActive+"/"+id] = raw
	}
	return kv
}

func ownersOf(t *testing.T, kv *activeKV, id string) StateBody {
	t.Helper()
	var b StateBody
	if err := json.Unmarshal(kv.vals[bus.BucketIntentActive+"/"+id], &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// submitN files n intents that do not overlap; their ids.
func submitN(t *testing.T, s *Service, n int) []string {
	t.Helper()
	var ids []string
	for i := range n {
		d, _, err := submit(t, s, with(baseRequest(), "client_ref", fmt.Sprintf("bf-%d", i),
			"volumes", []any{wireVolumeJSON(squareWire(41.0+float64(i)*0.1, 44.0, 0.01), 500, 550, t0, t1)}))
		if err != nil || d.Decision != DecisionAuthorised {
			t.Fatalf("submit %d: %v %s", i, err, d.Decision)
		}
		ids = append(ids, d.IntentID)
	}
	return ids
}

// The entries projected before this deploy are rewritten with their
// operator and client, exactly as the projection of their version writes
// them now and nothing published; a second pass rewrites nothing (E-01:
// the pass that finds nothing to do is run and read too).
func TestBackfillOwnersRewritesThePreDeployEntries(t *testing.T) {
	s, st, pr := newService(newRig())
	ids := submitN(t, s, 2)
	kv := preDeploy(t, pr)
	published := len(pr.subjects)

	res, err := s.BackfillOwners(t.Context(), kv, BackfillBatch)
	if err != nil || res.Rewritten != 2 || res.Left != 0 {
		t.Fatalf("first pass: %+v %v", res, err)
	}
	for _, id := range ids {
		got := ownersOf(t, kv, id)
		want := StateOf(st.byID[id])
		if got.OperatorID == "" || got.ClientID != testClient || got.OperatorID != want.OperatorID || got.Version != want.Version {
			t.Fatalf("%s: %+v", id, got)
		}
	}
	if len(pr.subjects) != published {
		t.Fatal("the backfill published an intent.v1 message")
	}
	if s.Counters.Get("intent_"+CounterOwnersBackfilled) != 2 {
		t.Fatalf("counters %v", s.Counters.Snapshot())
	}

	puts := kv.puts
	res, err = s.BackfillOwners(t.Context(), kv, BackfillBatch)
	if err != nil || res.Rewritten != 0 || res.Left != 0 || kv.puts != puts {
		t.Fatalf("second pass: %+v %v, %d puts", res, err, kv.puts-puts)
	}
}

// E-10: past its batch a pass leaves the rest, counted, for the next.
func TestBackfillOwnersIsBounded(t *testing.T) {
	s, _, pr := newService(newRig())
	submitN(t, s, 3)
	kv := preDeploy(t, pr)
	for i, want := range []BackfillResult{{Rewritten: 2, Left: 1}, {Rewritten: 1}, {}} {
		res, err := s.BackfillOwners(t.Context(), kv, 2)
		if err != nil || res != want {
			t.Fatalf("pass %d: %+v %v, want %+v", i, res, err, want)
		}
	}
	if s.Counters.Get("intent_"+CounterOwnersBackfillLeft) != 1 {
		t.Fatalf("counters %v", s.Counters.Snapshot())
	}
}

// An entry whose newest version is not projected yet is Republish's,
// not the backfill's; an intent that is no longer active is left to the
// projection that deletes it; an unreadable bucket is an error.
func TestBackfillOwnersLeavesWhatIsNotItsOwn(t *testing.T) {
	s, st, pr := newService(newRig())
	ids := submitN(t, s, 2)
	kv := preDeploy(t, pr)
	st.projected[ids[0]] = 0
	st.byID[ids[1]].LocalState = StateEnded
	res, err := s.BackfillOwners(t.Context(), kv, BackfillBatch)
	if err != nil || res.Rewritten != 0 || kv.puts != 0 {
		t.Fatalf("%+v %v, %d puts", res, err, kv.puts)
	}
	kv.keyErr = errors.New("nats: timeout")
	if _, err := s.BackfillOwners(t.Context(), kv, BackfillBatch); err == nil {
		t.Fatal("an unreadable intent_active passed")
	}
}

// The run at start stops once a pass leaves nothing behind; a pass that
// fails is tried again until the context ends (E-02: the dependency
// taken away and the run read).
func TestRunOwnerBackfillStopsWhenDone(t *testing.T) {
	s, _, pr := newService(newRig())
	ids := submitN(t, s, 1)
	kv := preDeploy(t, pr)
	s.RunOwnerBackfill(t.Context(), kv, time.Millisecond)
	if ownersOf(t, kv, ids[0]).ClientID != testClient {
		t.Fatal("the run did not backfill")
	}

	kv.keyErr = errors.New("nats: timeout")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	s.RunOwnerBackfill(ctx, kv, time.Millisecond)
	if ctx.Err() == nil {
		t.Fatal("a failing backfill stopped before its context ended")
	}
}
