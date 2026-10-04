//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/status"
	statusstore "github.com/rootxkit/uspace-ussp/internal/status/pgstore"
	"github.com/rootxkit/uspace-ussp/internal/store"
	"github.com/rootxkit/uspace-ussp/internal/testfakes/authority"
)

// The done-when status notices against the real database and the fake
// authority: the start is sent once and recorded; after a restart (a
// new service on the same database) asking again answers the stored
// notice and nothing is sent again; cease and restart follow on request;
// with the authority down a notice is tried again and recorded once it
// is back.
func TestIntegrationStatusNotices(t *testing.T) {
	ensureSchemas(t)
	ctx := context.Background()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	cert := hex.EncodeToString(b[:])
	fake := authority.New()
	t.Cleanup(fake.Close)
	client, err := status.NewClient(fake.URL(), authority.Tokens{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	newSvc := func() *status.Service {
		return &status.Service{Store: statusstore.Store{S: appStore(t)}, Authority: client, CertificateID: cert, SystemID: "USSP-DEV",
			Counters: &core.Counters{}, Logger: quiet()}
	}
	s := newSvc()
	start, created, err := s.Request(ctx, "staff-1", status.KindStart)
	if err != nil || !created {
		t.Fatal(created, err)
	}
	if s.SendDue(ctx) != 1 {
		t.Fatal("start not recorded")
	}
	restarted := newSvc()
	again, created, err := restarted.Request(ctx, "staff-2", status.KindStart)
	if err != nil || created || again.Reference != start.Reference || again.State != "delivered" {
		t.Fatalf("after a restart: %+v %v %v", again, created, err)
	}
	if restarted.SendDue(ctx) != 0 {
		t.Fatal("the start was sent again")
	}
	if _, created, err := restarted.Request(ctx, "staff-2", status.KindCease); err != nil || !created {
		t.Fatal(created, err)
	}
	fake.Down()
	if restarted.SendDue(ctx) != 0 {
		t.Fatal("recorded while the authority was down")
	}
	ns, err := restarted.List(ctx)
	if err != nil || len(ns) != 2 || ns[0].Kind != status.KindCease || ns[0].State != "pending" || ns[0].Attempts != 1 || ns[0].LastError == nil {
		t.Fatalf("while down: %+v %v", ns, err)
	}
	fake.Up()
	if _, err := appPool(t).Exec(ctx, "UPDATE operating_status_notices SET next_at = now() WHERE certificate_id = $1", cert); err != nil {
		t.Fatal(err)
	}
	if restarted.SendDue(ctx) != 1 {
		t.Fatal("cease not recorded once the authority was back")
	}
	if _, created, err := restarted.Request(ctx, "staff-2", status.KindRestart); err != nil || !created || restarted.SendDue(ctx) != 1 {
		t.Fatal("restart", created, err)
	}
	var mine []authority.StatusNotice
	for _, n := range fake.StatusNotices() {
		if n.CertificateID == cert {
			mine = append(mine, n)
		}
	}
	if len(mine) != 3 || mine[0].State != "started" || mine[1].State != "ceased" || mine[2].State != "restarted" {
		t.Fatalf("at the authority: %+v", mine)
	}
	if n := count(t, appPool(t), "SELECT count(*) FROM operating_status_notices WHERE certificate_id = $1 AND kind = 'start'", cert); n != 1 {
		t.Errorf("%d start notices stored", n)
	}
	if state, detail := restarted.Probe()(ctx); state != "up" {
		t.Errorf("readyz %s %s", state, detail)
	}
}

// A notice stored before 00021 has no certificate id; the backfill
// gives it a made-up one, so one never delivered is failed with the
// reason and never claimed, while one delivered stays delivered (E-01).
func TestIntegrationStatusBackfillFailsUndelivered(t *testing.T) {
	ensureSchemas(t)
	ctx := context.Background()
	owner := relOwner(t)
	if _, err := owner.MigrateDown(ctx, store.TreeRelational, 20); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := relOwner(t).Migrate(context.Background(), store.TreeRelational); err != nil {
			t.Errorf("restore the schema: %v", err)
		}
	})
	var pending, delivered string
	if err := owner.QueryRow(ctx, "INSERT INTO operating_status_notices (kind, at) VALUES ('cease', now()) RETURNING id::text").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `INSERT INTO operating_status_notices (kind, at, submitted_at, authority_ref)
		VALUES ('start', now(), now(), 'AUTH-1') RETURNING id::text`).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = relOwner(t).Exec(context.Background(), "DELETE FROM operating_status_notices WHERE id = ANY($1::uuid[])", []string{pending, delivered})
	})
	if _, err := owner.Migrate(ctx, store.TreeRelational); err != nil {
		t.Fatal(err)
	}
	var state string
	var failedAt *time.Time
	var lastError *string
	if err := owner.QueryRow(ctx, "SELECT state, failed_at, last_error FROM operating_status_notices WHERE id = $1::uuid", pending).
		Scan(&state, &failedAt, &lastError); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || failedAt == nil || lastError == nil || !strings.Contains(*lastError, "certificate id") {
		t.Errorf("an undelivered notice of before 00021: %s %v %v", state, failedAt, lastError)
	}
	if err := owner.QueryRow(ctx, "SELECT state FROM operating_status_notices WHERE id = $1::uuid", delivered).Scan(&state); err != nil || state != "delivered" {
		t.Errorf("a delivered notice of before 00021: %s %v", state, err)
	}
	if n := count(t, owner, "SELECT count(*) FROM operating_status_notices WHERE id = $1::uuid AND state = 'pending'", pending); n != 0 {
		t.Error("the undelivered notice is still pending")
	}
}

// Concurrent requests for the same change against the real database:
// eight consoles ask for a cease at once after the start, then for a
// restart; one of each is stored, every request answers it, and the
// notices stay start, cease, restart (00023's unique index).
func TestIntegrationStatusConcurrentRequests(t *testing.T) {
	ensureSchemas(t)
	ctx := context.Background()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	cert := hex.EncodeToString(b[:])
	svc := &status.Service{Store: statusstore.Store{S: appStore(t)}, CertificateID: cert, SystemID: "USSP-DEV",
		Counters: &core.Counters{}, Logger: quiet()}
	if _, _, err := svc.Request(ctx, "staff-0", status.KindStart); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{status.KindCease, status.KindRestart} {
		const consoles = 8
		refs := make([]string, consoles)
		errs := make([]error, consoles)
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for i := range consoles {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				n, _, err := svc.Request(ctx, fmt.Sprintf("staff-%d", i), kind)
				refs[i], errs[i] = n.Reference, err
			}()
		}
		close(gate)
		wg.Wait()
		for i := range consoles {
			if errs[i] != nil || refs[i] != refs[0] {
				t.Fatalf("%s: console %d answered %q %v, console 0 %q", kind, i, refs[i], errs[i], refs[0])
			}
		}
		if n := count(t, appPool(t), "SELECT count(*) FROM operating_status_notices WHERE certificate_id = $1 AND kind = $2", cert, kind); n != 1 {
			t.Fatalf("%d %s notices stored", n, kind)
		}
	}
	ns, err := svc.List(ctx)
	if err != nil || len(ns) != 3 || ns[0].Kind != status.KindRestart || ns[1].Kind != status.KindCease || ns[2].Kind != status.KindStart {
		t.Fatalf("the notices: %+v %v", ns, err)
	}
}
