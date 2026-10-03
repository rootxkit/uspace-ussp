package dss

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/f3548"

	"github.com/rootxkit/uspace-ussp/internal/obs"
)

// Probe is the strategic coordination part of the readiness entry dss:
// down since T while the DSS does not answer; degraded while the
// authority holds this USSP Down, while DSS work or notifications wait
// (depth and the oldest item's age in the detail), or while the DSS's
// clock differs from ours by more than TimeSyncMaxDifferentialSeconds;
// unknown before the first DSS call; up otherwise, with the availability
// in the detail.
func Probe(c *Client, a *Availability, st Store) obs.Probe {
	return func(ctx context.Context) (obs.State, string) {
		var parts []string
		if st != nil {
			n, age, attempts, err := st.Backlog(ctx, OIRKinds)
			switch {
			case err != nil:
				parts = append(parts, "intent outbox depth unknown")
			case n > 0:
				parts = append(parts, fmt.Sprintf("%d intent writes waiting, the oldest %.0f s, at most %d attempts", n, age, attempts))
			}
			n, age, _, err = st.Backlog(ctx, NotifyKinds)
			switch {
			case err != nil:
				parts = append(parts, "notification outbox depth unknown")
			case n > 0:
				parts = append(parts, fmt.Sprintf("%d notifications waiting, the oldest %.0f s", n, age))
			}
		}
		avail := "uss_availability unknown"
		down := false
		if a != nil {
			state, known, _, lastErr := a.State()
			switch {
			case known:
				avail = "uss_availability " + string(state)
				down = state == f3548.Down
			case lastErr != "":
				avail = "uss_availability not read: " + lastErr
			}
		}
		r := c.Reach()
		if r.DriftKnown && math.Abs(r.DriftS) > f3548.TimeSyncMaxDifferentialSeconds {
			parts = append(parts, fmt.Sprintf("the DSS's clock differs from ours by %.0f s (more than %d s)", r.DriftS, f3548.TimeSyncMaxDifferentialSeconds))
		}
		detail := strings.Join(append([]string{avail}, parts...), "; ")
		switch {
		case !r.Known:
			return obs.StateUnknown, "no F3548 DSS call yet; " + detail
		case !r.Up:
			return obs.StateDown, "F3548 DSS down since " + r.Since.Format(time.RFC3339) + ": " + r.Reason + "; " + detail
		case down:
			return obs.StateDegraded, "the authority set this USSP's availability Down: no new DSS write is made; " + detail
		case len(parts) > 0:
			return obs.StateDegraded, detail
		}
		return obs.StateUp, detail
	}
}

// Merge is one readiness entry from several: the worst state, the
// details of each joined, named.
func Merge(probes map[string]obs.Probe) obs.Probe {
	return func(ctx context.Context) (obs.State, string) {
		rank := map[obs.State]int{obs.StateUp: 0, obs.StateUnknown: 1, obs.StateDegraded: 2, obs.StateDown: 3}
		worst := obs.StateUp
		details := make([]string, 0, len(probes))
		for _, name := range sortedKeys(probes) {
			s, d := probes[name](ctx)
			if rank[s] > rank[worst] {
				worst = s
			}
			switch {
			case d == "" && s == obs.StateUp:
				continue
			case d == "":
				d = string(s)
			}
			details = append(details, name+": "+d)
		}
		return worst, strings.Join(details, " | ")
	}
}

func sortedKeys(m map[string]obs.Probe) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
