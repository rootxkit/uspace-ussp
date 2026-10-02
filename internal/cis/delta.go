package cis

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rootxkit/uspace-ussp/internal/cis/cispclient"
)

// errDeltaUnusable is the wrapped error of a delta that cannot be
// applied to the version held; the caller reads the dataset whole.
var errDeltaUnusable = errors.New("the delta does not apply to the version held")

// mergeDelta builds the collection of a DatasetDelta's to_version from
// the version held and its entries: removed first, then changed and
// added (the order DatasetDelta prescribes). Any inconsistency (another
// dataset, another from_version, a changed or removed identifier that
// is not held, an added one that is) is errDeltaUnusable: the caller
// reads the whole dataset instead, never a guess. The result goes
// through ParseVersion like any other body.
func mergeDelta(cur *Version, held []*Entry, raw []byte) ([]byte, int64, error) {
	var d cispclient.DatasetDelta
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, 0, fmt.Errorf("%w: %s", errDeltaUnusable, short(err.Error()))
	}
	var feats struct {
		Added struct {
			Features []json.RawMessage `json:"features"`
		} `json:"added"`
		Changed struct {
			Features []json.RawMessage `json:"features"`
		} `json:"changed"`
	}
	if err := json.Unmarshal(raw, &feats); err != nil {
		return nil, 0, fmt.Errorf("%w: %s", errDeltaUnusable, short(err.Error()))
	}
	switch {
	case string(d.Dataset) != string(cur.Dataset):
		return nil, 0, fmt.Errorf("%w: dataset %q", errDeltaUnusable, short(string(d.Dataset)))
	case d.FromVersion != cur.Number:
		return nil, 0, fmt.Errorf("%w: from_version %d, held %d", errDeltaUnusable, d.FromVersion, cur.Number)
	case d.ToVersion <= d.FromVersion:
		return nil, 0, fmt.Errorf("%w: to_version %d is not after %d", errDeltaUnusable, d.ToVersion, d.FromVersion)
	}
	order := make([]string, 0, len(held))
	byID := make(map[string]json.RawMessage, len(held))
	for _, e := range held {
		order = append(order, e.Identifier)
		byID[e.Identifier] = e.Raw
	}
	for _, id := range d.Removed {
		if _, ok := byID[id]; !ok {
			return nil, 0, fmt.Errorf("%w: removed %q is not held", errDeltaUnusable, short(id))
		}
		delete(byID, id)
	}
	for _, f := range feats.Changed.Features {
		id, err := identifierOf(f)
		if err != nil {
			return nil, 0, err
		}
		if _, ok := byID[id]; !ok {
			return nil, 0, fmt.Errorf("%w: changed %q is not held", errDeltaUnusable, short(id))
		}
		byID[id] = f
	}
	for _, f := range feats.Added.Features {
		id, err := identifierOf(f)
		if err != nil {
			return nil, 0, err
		}
		if _, ok := byID[id]; ok {
			return nil, 0, fmt.Errorf("%w: added %q is already held", errDeltaUnusable, short(id))
		}
		byID[id] = f
		order = append(order, id)
	}
	out := make([]json.RawMessage, 0, len(byID))
	for _, id := range order {
		if f, ok := byID[id]; ok {
			out = append(out, f)
			delete(byID, id) // an identifier removed and added again is listed once
		}
	}
	body, err := json.Marshal(struct {
		Type       string            `json:"type"`
		Features   []json.RawMessage `json:"features"`
		CISDataset string            `json:"cis_dataset"`
		CISVersion int64             `json:"cis_version"`
	}{Type: "FeatureCollection", Features: out, CISDataset: string(cur.Dataset), CISVersion: d.ToVersion})
	if err != nil {
		return nil, 0, err
	}
	return body, d.ToVersion, nil
}

func identifierOf(f json.RawMessage) (string, error) {
	var x struct {
		Properties struct {
			Identifier string `json:"identifier"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(f, &x); err != nil || x.Properties.Identifier == "" {
		return "", fmt.Errorf("%w: a feature without properties.identifier", errDeltaUnusable)
	}
	return x.Properties.Identifier, nil
}
