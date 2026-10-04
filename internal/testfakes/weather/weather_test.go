package weather

import (
	"net/http"
	"testing"
)

// Served signals each request and drops a signal beyond its bound
// without holding the request (E-10).
func TestServedIsBounded(t *testing.T) {
	f := New()
	t.Cleanup(f.Close)
	for range MaxServedSignals + 3 {
		resp, err := http.Get(f.URL() + "/metar?ids=UGTB&format=json")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if f.Calls() != MaxServedSignals+3 || len(f.Served()) != MaxServedSignals {
		t.Fatalf("%d calls, %d signals", f.Calls(), len(f.Served()))
	}
	<-f.Served()
	if len(f.Served()) != MaxServedSignals-1 {
		t.Fatal(len(f.Served()))
	}
}
