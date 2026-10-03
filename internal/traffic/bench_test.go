package traffic

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/rootxkit/uspace-core/geodesy"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/policy"
)

// BenchmarkTrafficProduct is one subscriber's product among 1000 tracks
// (docs/bench-targets.txt): the read of the picture in a 2 km area, the
// throttle and the encoding.
func BenchmarkTrafficProduct(b *testing.B) {
	pic := &Picture{}
	for i := 0; i < 1000; i++ {
		pic.Put(sample(fmt.Sprintf("t%d", i), geodesy.Destination(origin, float64(i%360), float64(10*i)), t0), []byte(`{}`))
	}
	v := policy.Defaults()
	a := area(v.TrafficRadiusM)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sel := pic.Tracks(a, t0.Add(time.Second), v, nil)
		kept, _ := Throttle(sel, v.TrafficThrottleTracks, uint64(i))
		p := Product{Envelope: bus.SystemEnvelope(SchemaProduct, ProducerWS, t0), Body: ProductBody{Tracks: Tracked(kept)}}
		if _, err := json.Marshal(&p); err != nil {
			b.Fatal(err)
		}
	}
}
