package deconflict

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/f3548"
	"github.com/rootxkit/uspace-core/geodesy"
	"github.com/rootxkit/uspace-core/geoid"
	"github.com/rootxkit/uspace-core/vectors"
)

// VectorsPath is the repo's deconfliction vector file.
const vectorsPath = "../../../testdata/vectors/deconfliction.json"

type vecIntent struct {
	ID          string           `json:"id"`
	Priority    int              `json:"priority"`
	FiledAt     time.Time        `json:"filed_at"`
	UndulationM float64          `json:"undulation_m"`
	Volumes     []f3548.Volume4D `json:"volumes"`
}

type pairInput struct {
	Check  string `json:"check"`
	Policy struct {
		BufferM         float64 `json:"buffer_m"`
		VerticalBufferM float64 `json:"vertical_buffer_m"`
	} `json:"policy"`
	A vecIntent `json:"a"`
	B vecIntent `json:"b"`
}

type pairExpected struct {
	Conflict *bool   `json:"conflict"`
	Winner   *string `json:"winner"`
	Rule     *string `json:"rule"`
	Error    *string `json:"error"`
}

// shapeOf maps a wire outline onto a Shape, the minimal reading the
// vectors need (internal/intent is the production adapter and runs the
// same cases through its own normalisation).
func shapeOf(t *testing.T, v f3548.Volume3D) Shape {
	t.Helper()
	switch {
	case v.OutlineCircle != nil:
		c := v.OutlineCircle
		return Shape{Circle: &geodesy.Circle{Center: c.Center.LatLon(), RadiusM: float64(c.Radius.Value)}}
	case v.OutlinePolygon != nil:
		out := make([]core.LatLon, len(v.OutlinePolygon.Vertices))
		for i, p := range v.OutlinePolygon.Vertices {
			out[i] = p.LatLon()
		}
		return Shape{Polygon: out}
	}
	t.Fatal("volume without an outline")
	return Shape{}
}

func intentOf(t *testing.T, in vecIntent) Intent {
	t.Helper()
	out := Intent{ID: in.ID, Priority: in.Priority, RankAt: in.FiledAt}
	for _, v := range in.Volumes {
		lo, err := v.Volume.AltitudeLower.HAEM()
		if err != nil {
			t.Fatal(err)
		}
		hi, err := v.Volume.AltitudeUpper.HAEM()
		if err != nil {
			t.Fatal(err)
		}
		out.Volumes = append(out.Volumes, Volume{
			Shape:      shapeOf(t, v.Volume),
			LowerAMSLM: geoid.AMSLFromHAE(lo, in.UndulationM), UpperAMSLM: geoid.AMSLFromHAE(hi, in.UndulationM),
			Start: v.TimeStart.Value, End: v.TimeEnd.Value,
		})
	}
	return out
}

func loadVectors(t *testing.T) *vectors.File {
	t.Helper()
	f, err := vectors.Read(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// The pair cases of the vector file, each judged both ways: A against
// B and B against A must give the same pair decision.
func TestVectorsDeconflictionPairs(t *testing.T) {
	f := loadVectors(t)
	ran := 0
	for _, c := range f.Cases {
		var probe struct {
			Check string `json:"check"`
		}
		if err := json.Unmarshal(c.Input, &probe); err != nil {
			t.Fatal(err)
		}
		if probe.Check != "pair" {
			continue
		}
		ran++
		t.Run(c.Name, func(t *testing.T) {
			t.Logf("why: %s", c.Why)
			var in pairInput
			var want pairExpected
			c.Decode(t, &in, &want)
			p := Policy{HorizontalBufferM: in.Policy.BufferM, VerticalBufferM: in.Policy.VerticalBufferM}
			a, b := intentOf(t, in.A), intentOf(t, in.B)
			ab, errAB := Check(a, []Intent{b}, p)
			ba, errBA := Check(b, []Intent{a}, p)
			if want.Error != nil {
				for _, err := range []error{errAB, errBA} {
					if err == nil || !strings.Contains(err.Error(), *want.Error) {
						t.Fatalf("error %v, want one naming %s", err, *want.Error)
					}
				}
				return
			}
			if errAB != nil || errBA != nil {
				t.Fatalf("errors %v / %v", errAB, errBA)
			}
			if (len(ab) == 1) != *want.Conflict || (len(ba) == 1) != *want.Conflict {
				t.Fatalf("conflict A-B %v B-A %v, want %v", ab, ba, *want.Conflict)
			}
			if !*want.Conflict {
				return
			}
			if ab[0].MineWins == ba[0].MineWins {
				t.Fatalf("both sides say the same thing about winning: %+v / %+v", ab[0], ba[0])
			}
			winner := "b"
			if ab[0].MineWins {
				winner = "a"
			}
			if winner != *want.Winner || ab[0].Rule != *want.Rule || ba[0].Rule != *want.Rule {
				t.Fatalf("winner %s rules %s/%s, want %s %s", winner, ab[0].Rule, ba[0].Rule, *want.Winner, *want.Rule)
			}
			if ab[0].Overlap != ba[0].Overlap && (ab[0].Overlap.VM != ba[0].Overlap.VM || ab[0].Overlap.TS != ba[0].Overlap.TS) {
				t.Errorf("overlap not symmetric: %+v / %+v", ab[0].Overlap, ba[0].Overlap)
			}
		})
	}
	if ran < 30 {
		t.Fatalf("%d pair cases ran, the brief asks for at least 30", ran)
	}
}
