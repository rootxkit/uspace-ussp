//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/rootxkit/uspace-core/core"

	"github.com/rootxkit/uspace-ussp/internal/status"
	statusstore "github.com/rootxkit/uspace-ussp/internal/status/pgstore"
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
	if _, err := relApp(t).Exec(ctx, "UPDATE operating_status_notices SET next_at = now() WHERE certificate_id = $1", cert); err != nil {
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
	if n := count(t, relApp(t), "SELECT count(*) FROM operating_status_notices WHERE certificate_id = $1 AND kind = 'start'", cert); n != 1 {
		t.Errorf("%d start notices stored", n)
	}
	if state, detail := restarted.Probe()(ctx); state != "up" {
		t.Errorf("readyz %s %s", state, detail)
	}
}
