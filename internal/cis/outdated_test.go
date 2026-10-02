package cis

import (
	"strings"
	"testing"
	"time"
)

// Outdated is empty while the versions in use are the newest the CISP
// published (E-02: the branch that says nothing is wrong), and names a
// newer version held untrusted, a newer publication refused and a
// notified version not pulled; a newer trusted version clears each.
func TestOutdatedNamesEveryKnownNewerVersion(t *testing.T) {
	g := newCacheRig(t, "")
	ctx := t.Context()
	for _, d := range ED318Datasets {
		g.fake.Publish(string(d), prohibited("TZP001").json())
		if err := g.cache.Pull(ctx, d, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	if out := g.cache.Outdated(); len(out) != 0 {
		t.Fatalf("current versions reported outdated: %v", out)
	}

	// Held: zones v2 has no publisher signature.
	g.fake.Publish("zones", prohibited("TZP002").json())
	g.fake.SetPublisherSignature("zones", 2, "", "")
	g.pullHeld(t, Zones, 2, "no X-Publisher-Signature")
	if out := g.cache.Outdated(); len(out) != 1 || !strings.Contains(out[0], "zones version 2 held") {
		t.Fatalf("held: %v", out)
	}

	// Refused: restrictions v2 does not parse.
	bad := strings.Replace(string(collection(Restrictions, 2, prohibited("DAR0002").json())), `"PROHIBITED"`, `"FORBIDDEN"`, 1)
	g.fake.PublishRaw("restrictions", []byte(bad))
	if err := g.cache.Pull(ctx, Restrictions, nil, false); err == nil {
		t.Fatal("a malformed publication was accepted")
	}
	if out := strings.Join(g.cache.Outdated(), " | "); !strings.Contains(out, "a newer restrictions publication") {
		t.Fatalf("refused: %s", out)
	}

	// Notified and not pulled: uspace_airspace v5.
	g.cache.Trigger(USpaceAirspace, Hint{Version: 5, Issuer: "cisp", At: time.Now()})
	if out := strings.Join(g.cache.Outdated(), " | "); !strings.Contains(out, "uspace_airspace version 5 is notified and not pulled yet") {
		t.Fatalf("pending: %s", out)
	}

	// Newer trusted versions clear the held and the refused ones; the
	// notification is cleared by the version it named.
	g.fake.Publish("zones", prohibited("TZP003").json())
	g.fake.Publish("restrictions", prohibited("DAR0003").json())
	for _, d := range []Dataset{Zones, Restrictions} {
		if err := g.cache.Pull(ctx, d, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	for range 4 {
		g.fake.Publish("uspace_airspace", prohibited("TZP004").json())
	}
	if err := g.cache.Pull(ctx, USpaceAirspace, nil, false); err != nil {
		t.Fatal(err)
	}
	if out := g.cache.Outdated(); len(out) != 0 {
		t.Fatalf("still outdated after newer trusted versions: %v", out)
	}
}
