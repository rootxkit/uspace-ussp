package store

import (
	"encoding/json"
	"testing"

	"github.com/rootxkit/uspace-ussp/internal/policy"
	"github.com/rootxkit/uspace-ussp/internal/store/relational"
)

// A stored version that predates a threshold (WP-5's registry TTLs)
// loads with that threshold's default, so it still validates; a stored
// field this build does not know is still refused (E-01 pair).
func TestPolicyRecordFillsNewerFieldsWithDefaults(t *testing.T) {
	var old map[string]any
	raw, _ := json.Marshal(policy.Defaults())
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	delete(old, "registry_positive_ttl_s")
	delete(old, "registry_negative_ttl_s")
	old["cis_stale_s"] = 120.0
	raw, _ = json.Marshal(old)
	r, err := policyRecord(relational.Policy{Version: 3, Values: raw})
	if err != nil {
		t.Fatal(err)
	}
	if r.Values.RegistryPositiveTTLS != 86400 || r.Values.RegistryNegativeTTLS != 300 || r.Values.CISStaleS != 120 || r.Values.Validate() != nil {
		t.Fatalf("loaded %+v", r.Values)
	}
	old["registry_name_ttl_s"] = 1.0
	raw, _ = json.Marshal(old)
	if _, err := policyRecord(relational.Policy{Version: 4, Values: raw}); err == nil {
		t.Fatal("an unknown stored field was accepted")
	}
}
