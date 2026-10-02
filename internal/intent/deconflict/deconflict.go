package deconflict

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/rootxkit/uspace-core/core"
)

// Bounds of one check (E-10, Z-06): a larger input is refused, never cut.
const (
	// MaxVolumes bounds the volumes of one intent.
	MaxVolumes = 64
	// MaxOthers bounds the intents one intent is checked against
	// (docs/PLAN.md §9: at most 1000 active local plus peer intents).
	MaxOthers = 5000
)

// The precedence rules a conflict names.
const (
	RulePriority       = "priority"
	RuleFirstComeFirst = "first_come_first_served"
)

// Policy is the thresholds of the judgement, from the policy row
// (deconflict_buffer_m, deconflict_vertical_buffer_m).
type Policy struct {
	HorizontalBufferM float64
	VerticalBufferM   float64
}

// Validate refuses a buffer that is negative or not finite (E-15: an
// invalid threshold refuses the check, never disarms it); 0 is no
// buffer.
func (p Policy) Validate() error {
	for _, f := range []struct {
		name string
		v    float64
	}{{"deconflict_buffer_m", p.HorizontalBufferM}, {"deconflict_vertical_buffer_m", p.VerticalBufferM}} {
		if !core.IsFinite(f.v) || f.v < 0 {
			return core.Fieldf(f.name, "must be a finite number of at least 0, got %v", f.v)
		}
	}
	return nil
}

// Volume is one Volume4D as the judgement uses it.
type Volume struct {
	Shape Shape
	// LowerAMSLM and UpperAMSLM are the band in AMSL (D-01).
	LowerAMSLM, UpperAMSLM float64
	// Start and End bound the volume in time, both included.
	Start, End time.Time
}

func (v Volume) check(field string) error {
	if err := v.Shape.check(field + ".outline"); err != nil {
		return err
	}
	if !core.IsFinite(v.LowerAMSLM) || !core.IsFinite(v.UpperAMSLM) || v.LowerAMSLM > v.UpperAMSLM {
		return core.Fieldf(field+".band", "lower %v and upper %v AMSL are not an ordered pair of finite numbers", v.LowerAMSLM, v.UpperAMSLM)
	}
	if v.Start.IsZero() || v.End.IsZero() || v.End.Before(v.Start) {
		return core.Fieldf(field+".time", "start %v and end %v are not an ordered pair of instants", v.Start, v.End)
	}
	return nil
}

// Intent is one operational intent as the judgement uses it.
type Intent struct {
	ID       string
	Priority int
	// RankAt is when the volumes judged were filed: the creation of the
	// intent, or the modification that brought them (first come, first
	// served, Art. 10(9)).
	RankAt  time.Time
	Volumes []Volume
}

func (in Intent) check(field string) error {
	if in.ID == "" {
		return core.Fieldf(field+".id", "required")
	}
	if in.RankAt.IsZero() {
		return core.Fieldf(field+".rank_at", "required")
	}
	if len(in.Volumes) == 0 || len(in.Volumes) > MaxVolumes {
		return core.Fieldf(field+".volumes", "has %d volumes; 1 to %d", len(in.Volumes), MaxVolumes)
	}
	for i, v := range in.Volumes {
		if err := v.check(fmt.Sprintf("%s.volumes[%d]", field, i)); err != nil {
			return err
		}
	}
	return nil
}

// Precedes reports whether a has precedence over b, and the rule that
// decides it: the higher priority (Art. 10(8)); at equal priority the
// earlier RankAt, then the smaller id (Art. 10(9)). It is antisymmetric
// for two different ids: exactly one of Precedes(a, b) and Precedes(b, a)
// is true.
func Precedes(a, b Intent) (bool, string) {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority, RulePriority
	}
	if !a.RankAt.Equal(b.RankAt) {
		return a.RankAt.Before(b.RankAt), RuleFirstComeFirst
	}
	return a.ID < b.ID, RuleFirstComeFirst
}

// Overlap is how two conflicting volumes meet: the horizontal separation
// in metres (0 when the outlines meet; at most the buffer otherwise),
// the vertical overlap of the AMSL bands in metres (negative when they
// are apart but within the vertical buffer) and the common time in
// seconds (0 when the windows touch).
type Overlap struct {
	HM float64 `json:"h_m"`
	VM float64 `json:"v_m"`
	TS float64 `json:"t_s"`
}

// VolumesConflict reports whether two checked volumes meet in space,
// time and altitude under p, with how they meet.
func VolumesConflict(a, b Volume, p Policy) (bool, Overlap, error) {
	if err := p.Validate(); err != nil {
		return false, Overlap{}, err
	}
	if err := a.check("a"); err != nil {
		return false, Overlap{}, err
	}
	if err := b.check("b"); err != nil {
		return false, Overlap{}, err
	}
	return volumes(a, b, p)
}

func volumes(a, b Volume, p Policy) (bool, Overlap, error) {
	if a.Start.After(b.End) || b.Start.After(a.End) {
		return false, Overlap{}, nil
	}
	if a.LowerAMSLM > b.UpperAMSLM+p.VerticalBufferM || b.LowerAMSLM > a.UpperAMSLM+p.VerticalBufferM {
		return false, Overlap{}, nil
	}
	ok, sep, err := Within(a.Shape, b.Shape, p.HorizontalBufferM)
	if err != nil || !ok {
		return false, Overlap{}, err
	}
	start := a.Start
	if b.Start.After(start) {
		start = b.Start
	}
	end := a.End
	if b.End.Before(end) {
		end = b.End
	}
	return true, Overlap{
		HM: sep,
		VM: math.Min(a.UpperAMSLM, b.UpperAMSLM) - math.Max(a.LowerAMSLM, b.LowerAMSLM),
		TS: end.Sub(start).Seconds(),
	}, nil
}

// Conflict is one other intent that conflicts with the intent checked:
// the first pair of volumes that meet (by index), how they meet, and the
// precedence between the two intents.
type Conflict struct {
	OtherID     string
	MineVolume  int
	OtherVolume int
	Overlap     Overlap
	// MineWins is true when the intent checked has precedence: the other
	// is an authorisation to flag for an update (Art. 10(10)); false
	// when the intent checked loses and is rejected.
	MineWins bool
	Rule     string
}

// Check judges mine against every other intent (an intent with mine's
// id is skipped: it is mine's previous version) and returns the
// conflicts sorted by the other's id. An invalid policy, an intent that
// cannot be judged, or more than MaxOthers others is an error: the
// check did not run, which is never "no conflict".
func Check(mine Intent, others []Intent, p Policy) ([]Conflict, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if len(others) > MaxOthers {
		return nil, core.Fieldf("others", "%d intents to check against; at most %d", len(others), MaxOthers)
	}
	if err := mine.check("intent"); err != nil {
		return nil, err
	}
	var out []Conflict
	for k, o := range others {
		if o.ID == mine.ID {
			continue
		}
		if err := o.check(fmt.Sprintf("others[%d]", k)); err != nil {
			return nil, err
		}
		c, ok, err := pair(mine, o, p)
		if err != nil {
			return nil, fmt.Errorf("against %s: %w", o.ID, err)
		}
		if ok {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b Conflict) int { return cmp.Compare(a.OtherID, b.OtherID) })
	return out, nil
}

func pair(mine, o Intent, p Policy) (Conflict, bool, error) {
	for i, a := range mine.Volumes {
		for j, b := range o.Volumes {
			ok, ov, err := volumes(a, b, p)
			if err != nil {
				return Conflict{}, false, err
			}
			if ok {
				wins, rule := Precedes(mine, o)
				return Conflict{OtherID: o.ID, MineVolume: i, OtherVolume: j, Overlap: ov, MineWins: wins, Rule: rule}, true, nil
			}
		}
	}
	return Conflict{}, false, nil
}
