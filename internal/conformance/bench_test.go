package conformance

import (
	"testing"

	"github.com/rootxkit/uspace-core/core"
	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/intent/deconflict"
)

// BenchmarkConformanceJudge is one sample against a 20-vertex polygon
// volume (docs/bench-targets.txt: <= 50 us per sample, spec 05 §7).
func BenchmarkConformanceJudge(b *testing.B) {
	a := circleAuth()
	poly := make([]core.LatLon, 20)
	for i := range poly {
		poly[i] = geodesy.Destination(origin, float64(i)*18, 500)
	}
	a.Volumes[0].Shape = deconflict.Shape{Polygon: poly}
	s := Sample{Position: geodesy.Destination(origin, 45, 650), AltAMSLM: alt(550), AltSource: core.AltGeodetic, CapturedAt: tt0}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Judge(s, a, Policy{PressureUncertaintyM: 250}); err != nil {
			b.Fatal(err)
		}
	}
}
