//go:build integration && lab

package integration

import (
	"testing"

	"github.com/rootxkit/uspace-ussp/internal/auth"
)

// TestLabIdentityFollowsTheLabConfig: the id the api is expected to ask
// as is derived from the lab's USSP code, not a code fixed in the test,
// and a lab whose client is that id is a run like one whose client
// differs (a lab client in another case, such as ussp-dev01-01).
func TestLabIdentityFollowsTheLabConfig(t *testing.T) {
	for _, c := range []struct {
		code, lab   string
		wantBridged bool
	}{
		{"DEV01", "ussp-dev01-01", true},
		{"DEV01", auth.ClientIDFor("DEV01"), false},
		{"ABC1", auth.ClientIDFor("ABC1"), false},
		{"ABC1", "ussp-DEV01-01", true},
	} {
		derived, bridged := labIdentity(c.code, c.lab)
		if derived != auth.ClientIDFor(c.code) || bridged != c.wantBridged {
			t.Errorf("code %s, lab client %s: derived %s, bridged %v; want %s, %v",
				c.code, c.lab, derived, bridged, auth.ClientIDFor(c.code), c.wantBridged)
		}
	}
}
