package coordination

import (
	"context"
	"fmt"
	"time"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// DepANSP is the readiness dependency of the Annex V coordination.
const DepANSP = "ansp_coordination"

// PendingLate is how long a notice may wait for its receipt before
// /readyz calls the coordination degraded (02 F13: the ANSP is told
// within 5 s of a deviation, M2).
const PendingLate = 10 * time.Second

// MaxListed bounds the notices the console lists at once (E-10).
const MaxListed = 500

// Probe is the readiness of the coordination: up when no notice is
// failed or escalated, none is being tried again and none has waited
// longer than PendingLate; degraded otherwise, with the counts and the
// age of the oldest pending notice; degraded too when unconfigured names
// why no ANSP is called (the notices are queued and not sent; "" when
// one is). A store that cannot be read is unknown, never up.
func Probe(st Store, unconfigured string) obs.Probe {
	return func(ctx context.Context) (obs.State, string) {
		s, err := st.Summarise(ctx)
		if err != nil {
			return obs.StateUnknown, "the coordination notices cannot be read: " + clip(err.Error())
		}
		detail := fmt.Sprintf("%d pending (%d retrying, oldest %.0f s), %d escalated, %d failed", s.Pending, s.Retrying, s.OldestPendingAgeS, s.Escalated, s.Failed)
		switch {
		case unconfigured != "":
			return obs.StateDegraded, unconfigured + ": Annex V notices are queued and not sent; " + detail
		case s.Escalated > 0 || s.Failed > 0 || s.Retrying > 0 || (s.Pending > 0 && s.OldestPendingAgeS > PendingLate.Seconds()):
			return obs.StateDegraded, detail
		}
		return obs.StateUp, detail
	}
}
