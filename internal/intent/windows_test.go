package intent

import "testing"

// The geo query of an intent reads the caller's own intent only.
func TestWindowsOwnIntentOnly(t *testing.T) {
	s, st, _ := newService(newRig())
	d, _, err := submit(t, s, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	boxes, from, to, err := s.Windows(t.Context(), testClient, d.IntentID)
	if err != nil || len(boxes) != 1 || !from[0].Equal(t0) || !to[0].Equal(t1) || boxes[0].MinLat > 41.7001 {
		t.Fatalf("%v %v %v %v", boxes, from, to, err)
	}
	st.byID[d.IntentID].OperatorID = "another"
	if _, _, _, err := s.Windows(t.Context(), testClient, d.IntentID); err == nil {
		t.Fatal("another operator's intent answered")
	}
	if _, _, _, err := s.Windows(t.Context(), testClient, "x"); err == nil {
		t.Fatal("a malformed id answered")
	}
}
