package cis

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rootxkit/uspace-core/ed318"
)

// servedOnlyPrefix starts the members the CISP writes itself (top-level
// cis_dataset, cis_version, cis_updated_at, cis_publisher_stale_since;
// extendedProperties cis_restriction): a publisher never sends one, so
// they are outside what its signature covers.
const servedOnlyPrefix = "cis_"

// errNotSigned is the reason of a hold whose served content is not what
// the publisher signed.
const errNotSigned = "the features served are not the ones its publisher signed"

// signedMatches says whether v, the version as served (or as built from
// a delta) and about to be installed, carries what its publisher signed
// in signed, the bytes of GET /v1/{dataset}/versions/{v} that the
// publisher signature was verified over. The CISP serves a snapshot of
// the publication with its own cis_* members and its own metadata, so
// the comparison is of content, not bytes:
//
//   - an ED-318 collection: the same features by identifier, each equal
//     as ed318.Export writes it, extendedProperties cis_* left out;
//   - a restrictions version, whose signed bytes are the ANSP's request
//     for one restriction: the feature it carries is in the version,
//     equal as above (a request without a feature, an activate, an end
//     or a cancel, binds no feature);
//   - the ussp_list: the same document with the top-level cis_* members
//     left out.
//
// It returns "" when they match and the reason of the hold otherwise.
func signedMatches(v *Version, signed []byte) string {
	if v.Dataset == USSPList {
		return ussplistMatches(v.Body, signed)
	}
	if v.Collection == nil {
		return "the version holds no collection to compare"
	}
	served, err := canonicalFeatures(v.Collection.Features)
	if err != nil {
		return "the version's features do not export: " + short(err.Error())
	}
	if fc, probs := ed318.Parse(signed, ProblemLimits); probs == nil {
		want, err := canonicalFeatures(fc.Features)
		if err != nil {
			return "the signed features do not export: " + short(err.Error())
		}
		if len(want) != len(served) {
			return fmt.Sprintf("%s: %d signed, %d served", errNotSigned, len(want), len(served))
		}
		for id, w := range want {
			if s, ok := served[id]; !ok || !bytes.Equal(s, w) {
				return fmt.Sprintf("%s: %q differs", errNotSigned, short(id))
			}
		}
		return ""
	}
	if v.Dataset != Restrictions {
		return errNotSigned + ": the signed bytes are not an ED-318 collection"
	}
	var req struct {
		Feature json.RawMessage `json:"feature"`
	}
	if err := json.Unmarshal(signed, &req); err != nil {
		return errNotSigned + ": the signed bytes are neither an ED-318 collection nor a restriction request"
	}
	if len(req.Feature) == 0 || bytes.Equal(req.Feature, []byte("null")) {
		return ""
	}
	one, err := json.Marshal(map[string]any{"type": "FeatureCollection", "features": []json.RawMessage{req.Feature}})
	if err != nil {
		return errNotSigned + ": " + short(err.Error())
	}
	fc, probs := ed318.Parse(one, ProblemLimits)
	if probs != nil || len(fc.Features) != 1 {
		return errNotSigned + ": the signed request's feature does not parse"
	}
	want, err := canonicalFeatures(fc.Features)
	if err != nil {
		return "the signed feature does not export: " + short(err.Error())
	}
	for id, w := range want {
		if s, ok := served[id]; !ok || !bytes.Equal(s, w) {
			return fmt.Sprintf("%s: %q differs", errNotSigned, short(id))
		}
	}
	return ""
}

// canonicalFeatures is every feature by identifier as ed318.Export
// writes it with the extendedProperties cis_* members left out, in a
// canonical JSON form (members sorted). A repeated identifier is an
// error.
func canonicalFeatures(fs []ed318.Feature) (map[string][]byte, error) {
	clean := make([]ed318.Feature, len(fs))
	for i := range fs {
		f := fs[i]
		if f.Properties.ExtendedProperties != nil {
			ext := make(map[string]json.RawMessage, len(f.Properties.ExtendedProperties))
			for k, raw := range f.Properties.ExtendedProperties {
				if !strings.HasPrefix(k, servedOnlyPrefix) {
					ext[k] = raw
				}
			}
			if len(ext) == 0 {
				ext = nil
			}
			f.Properties.ExtendedProperties = ext
		}
		clean[i] = f
	}
	raw, err := exportFeatures(&ed318.FeatureCollection{Type: "FeatureCollection", Features: clean})
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(raw))
	for i, r := range raw {
		id := clean[i].Properties.Identifier
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("identifier %q twice", short(id))
		}
		c, err := canonicalJSON(r, false)
		if err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, nil
}

// ussplistMatches compares two ussp_list documents without their
// top-level cis_* members.
func ussplistMatches(served, signed []byte) string {
	s, err := canonicalJSON(served, true)
	if err != nil {
		return "the version does not read as JSON"
	}
	w, err := canonicalJSON(signed, true)
	if err != nil {
		return errNotSigned + ": the signed bytes are not JSON"
	}
	if !bytes.Equal(s, w) {
		return "the document served is not the one its publisher signed"
	}
	return ""
}

// canonicalJSON re-encodes raw with object members sorted (numbers kept
// as written); dropServed leaves the top-level cis_* members out.
func canonicalJSON(raw []byte, dropServed bool) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if m, ok := v.(map[string]any); ok && dropServed {
		for k := range m {
			if strings.HasPrefix(k, servedOnlyPrefix) {
				delete(m, k)
			}
		}
	}
	return json.Marshal(v)
}
