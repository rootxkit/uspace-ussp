//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	coresources "github.com/rootxkit/uspace-core/sources"

	"github.com/rootxkit/uspace-ussp/internal/httpx"
	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/sources"
)

type sourceProjector struct {
	mu   sync.Mutex
	err  error
	seen []coresources.State
}

func (p *sourceProjector) ProjectSources(_ context.Context, s coresources.State) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, s)
	return p.err
}

// Source switches: a refused projection leaves no row and no event
// (503-shaped); an accepted one writes the row, the event and a state
// with the database's epoch and a higher version, which a core follower
// applies; Republish publishes a still higher version, and the follower
// ignores an older state (core sources semantics).
func TestIntegrationSourceSwitch(t *testing.T) {
	ensureSchemas(t)
	ctx := context.Background()
	st := appStore(t)
	app := relApp(t)
	proj := &sourceProjector{err: errors.New("kv: bucket source_control unavailable")}
	w := &sources.Writer{Store: st, Projector: proj}
	instance := fmt.Sprint("receiver-", time.Now().UnixNano())
	c := coresources.Control{SourceType: "econspicuity", InstanceID: &instance, Enabled: false}
	rowsFor := func() int64 {
		return count(t, app, "SELECT count(*) FROM source_controls WHERE source_type = 'econspicuity' AND instance_id = $1", instance)
	}
	eventsFor := func() int64 {
		return count(t, app, "SELECT count(*) FROM events WHERE entity_type = 'source_control' AND entity_id = $1", "econspicuity/"+instance)
	}

	_, err := w.Switch(ctx, "staff-1", "receiver maintenance", c)
	var pe *policy.ProjectionError
	if !errors.As(err, &pe) || pe.Bucket != sources.BucketSourceControl || httpx.ProblemFromError(err).Status != 503 {
		t.Fatalf("failing projector: %v", err)
	}
	if rowsFor() != 0 || eventsFor() != 0 || w.Counters.Get(sources.CounterProjectionFailed) != 1 {
		t.Fatalf("refused switch left %d rows, %d events", rowsFor(), eventsFor())
	}

	proj.err = nil
	s1, err := w.Switch(ctx, "staff-1", "receiver maintenance", c)
	if err != nil {
		t.Fatal(err)
	}
	var epoch string
	if err := app.QueryRow(ctx, "SELECT epoch FROM source_control_epoch").Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if rowsFor() != 1 || eventsFor() != 1 || s1.Epoch != epoch || s1.Version <= proj.seen[0].Version {
		t.Fatalf("accepted switch: rows %d events %d state %+v epoch %s", rowsFor(), eventsFor(), s1, epoch)
	}
	f := coresources.NewFollower()
	if !f.Apply(s1) {
		t.Fatal("follower refused the first state")
	}
	if d := f.Query("econspicuity", &instance); d.Enabled || d.WhyDisabled == nil || *d.WhyDisabled != coresources.WhyInstance {
		t.Fatalf("follower decision %+v", d)
	}

	re, err := w.Republish(ctx)
	if err != nil || re.Version <= s1.Version || re.Epoch != epoch || !slices.ContainsFunc(re.Controls, func(x coresources.Control) bool {
		return x.InstanceID != nil && *x.InstanceID == instance && !x.Enabled
	}) {
		t.Fatalf("republish %+v %v", re, err)
	}
	if !f.Apply(re) || f.Apply(s1) {
		t.Fatal("follower: the republished state must apply and the older one must not")
	}
	if eventsFor() != 1 {
		t.Errorf("republish wrote an event: %d", eventsFor())
	}

	rows, state, err := w.List(ctx)
	if err != nil || state.Epoch != epoch || !slices.ContainsFunc(rows, func(r sources.Row) bool {
		return r.Control.InstanceID != nil && *r.Control.InstanceID == instance && r.Reason == "receiver maintenance" && r.Actor == "staff-1"
	}) {
		t.Fatalf("list %v %+v %v", rows, state, err)
	}

	// Invalid switches never reach the database.
	empty := ""
	if _, err := w.Switch(ctx, "", "", coresources.Control{InstanceID: &empty}); err == nil ||
		!strings.Contains(err.Error(), "source_type") || !strings.Contains(err.Error(), "instance_id") {
		t.Fatalf("invalid switch: %v", err)
	}
}
