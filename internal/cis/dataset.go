package cis

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rootxkit/uspace-core/ed269"
	"github.com/rootxkit/uspace-core/ed318"

	"github.com/rootxkit/uspace-ussp/internal/cis/cispclient"
)

// Dataset is one of the CISP's datasets (spec 02 F3, the DatasetPath
// enumeration of api/clients/cisp.yaml).
type Dataset string

// The datasets this USSP subscribes to and caches.
const (
	Zones          Dataset = "zones"
	USpaceAirspace Dataset = "uspace_airspace"
	Restrictions   Dataset = "restrictions"
	USSPList       Dataset = "ussp_list"
)

// ED318Datasets are the datasets that are ED-318 FeatureCollections: the
// ones geo-awareness rests on, and the ones Age covers.
var ED318Datasets = []Dataset{Zones, USpaceAirspace, Restrictions}

// AllDatasets is every dataset, in a fixed order.
var AllDatasets = []Dataset{Zones, USpaceAirspace, Restrictions, USSPList}

// ParseDataset returns the dataset named s, or false.
func ParseDataset(s string) (Dataset, bool) {
	for _, d := range AllDatasets {
		if string(d) == s {
			return d, true
		}
	}
	return "", false
}

// ED318 reports whether the dataset is an ED-318 FeatureCollection.
func (d Dataset) ED318() bool { return d != USSPList }

// ProblemLimits are the limits a dataset version is parsed with:
// ed269.DefaultLimits (brief WP-4), so the bytes a version may have are
// ed269.DefaultLimits.MaxBytes.
var ProblemLimits = ed269.DefaultLimits

// Metadata is what this USSP keeps about a version beside its features
// (cis_datasets.metadata): core's ed318.Metadata issued and provider,
// and the CISP's top-level cis_dataset, cis_version, cis_updated_at
// (M15), and for restrictions cis_publisher_stale_since while the ANSP
// is stale. For ussp_list the list itself is kept here.
type Metadata struct {
	Issued       *time.Time `json:"issued,omitempty"`
	Provider     []Text     `json:"provider,omitempty"`
	CISDataset   string     `json:"cis_dataset"`
	CISVersion   int64      `json:"cis_version"`
	CISUpdatedAt *time.Time `json:"cis_updated_at,omitempty"`
	// PublisherStaleSince is cis_publisher_stale_since as served (null:
	// the ANSP was never heard from); absent while the ANSP is current.
	PublisherStaleSince json.RawMessage `json:"cis_publisher_stale_since,omitempty"`
	// Delta is true for a version built from the previous one and the
	// CISP's delta (pull_url), false for a version read whole.
	Delta bool `json:"delta,omitempty"`
	// USSPList is the ussp_list document as served.
	USSPList json.RawMessage `json:"ussp_list,omitempty"`
}

// Text is one ED-318 text with its language.
type Text struct {
	Text string `json:"text"`
	Lang string `json:"lang"`
}

// Version is one accepted version of a dataset.
type Version struct {
	Dataset Dataset
	Number  int64
	ETag    string
	// Body is the bytes accepted (the dataset as served, or as built
	// from a delta).
	Body []byte
	// Collection is the parsed ED-318 collection (nil for ussp_list).
	Collection *ed318.FeatureCollection
	// USSPList is the parsed list (nil for the ED-318 datasets).
	USSPList *cispclient.UsspList
	Meta     Metadata
	// SignatureOK is whether a signature over the bytes was verified.
	// WP-4 verifies none: see docs/WORKPACKAGES/WP-4.md and the PR
	// (the CISP's X-CIS-Signature is kept per version with the iat of
	// its first serve, which core's detached verifier bounds to 5 min).
	SignatureOK bool
}

// RefusalError is a dataset version refused whole: the first problem by JSON
// path, every problem core listed (at most ProblemLimits.MaxProblems,
// "field: reason") and how many there were. The previous version is
// kept.
type RefusalError struct {
	Dataset  Dataset
	Version  int64
	First    string
	List     []string
	Problems int
}

func (r *RefusalError) Error() string {
	v := ""
	if r.Version > 0 {
		v = " version " + strconv.FormatInt(r.Version, 10)
	}
	return fmt.Sprintf("%s%s refused: %s (%d problems)", r.Dataset, v, r.First, r.Problems)
}

func refuse(d Dataset, version int64, first string) *RefusalError {
	return &RefusalError{Dataset: d, Version: version, First: first, List: []string{first}, Problems: 1}
}

// ParseVersion validates a dataset body as served and returns the
// version, or a *RefusalError. An ED-318 dataset goes through ed318.Parse
// with ProblemLimits (accepted whole or refused whole, never repaired,
// spec 06 T9); its top-level cis_dataset must name d and cis_version
// must be a version (at least 1). headerVersion, when above 0, is the
// X-CIS-Version the CISP sent and must equal cis_version.
func ParseVersion(d Dataset, body []byte, etag string, headerVersion int64) (*Version, *RefusalError) {
	if d == USSPList {
		return parseUSSPList(body, etag, headerVersion)
	}
	fc, probs := ed318.Parse(body, ProblemLimits)
	if probs != nil {
		r := &RefusalError{Dataset: d, Problems: len(probs.List) + probs.Truncated, First: "the collection does not parse"}
		for _, p := range probs.List {
			r.List = append(r.List, p.Field+": "+p.Reason)
		}
		if len(r.List) > 0 {
			r.First = r.List[0]
		}
		return nil, r
	}
	meta, rf := topLevel(d, fc.Extra, headerVersion)
	if rf != nil {
		return nil, rf
	}
	if fc.Metadata != nil {
		if fc.Metadata.Issued != nil {
			t := fc.Metadata.Issued.Time.UTC()
			meta.Issued = &t
		}
		for _, p := range fc.Metadata.Provider {
			if p.Text != nil {
				meta.Provider = append(meta.Provider, Text{Text: *p.Text, Lang: p.Lang})
			}
		}
	}
	return &Version{Dataset: d, Number: meta.CISVersion, ETag: etag, Body: body, Collection: fc, Meta: meta}, nil
}

// topLevel reads the CISP's cis_* members of a served collection.
func topLevel(d Dataset, extra map[string]json.RawMessage, headerVersion int64) (Metadata, *RefusalError) {
	var m Metadata
	if err := json.Unmarshal(extra["cis_dataset"], &m.CISDataset); err != nil || m.CISDataset == "" {
		return m, refuse(d, 0, "cis_dataset: missing or not a string")
	}
	if m.CISDataset != string(d) {
		return m, refuse(d, 0, fmt.Sprintf("cis_dataset: %q is not %q", short(m.CISDataset), d))
	}
	if err := json.Unmarshal(extra["cis_version"], &m.CISVersion); err != nil || m.CISVersion < 1 {
		return m, refuse(d, 0, "cis_version: missing or not an integer of at least 1")
	}
	if headerVersion > 0 && headerVersion != m.CISVersion {
		return m, refuse(d, m.CISVersion, fmt.Sprintf("cis_version: %d is not X-CIS-Version %d", m.CISVersion, headerVersion))
	}
	if raw, ok := extra["cis_updated_at"]; ok {
		var t time.Time
		if err := json.Unmarshal(raw, &t); err != nil {
			return m, refuse(d, m.CISVersion, "cis_updated_at: not an RFC 3339 time")
		}
		t = t.UTC()
		m.CISUpdatedAt = &t
	}
	if raw, ok := extra["cis_publisher_stale_since"]; ok {
		m.PublisherStaleSince = bytes.Clone(raw)
	}
	return m, nil
}

func parseUSSPList(body []byte, etag string, headerVersion int64) (*Version, *RefusalError) {
	if len(body) > ProblemLimits.MaxBytes {
		return nil, refuse(USSPList, 0, fmt.Sprintf("body: longer than %d bytes", ProblemLimits.MaxBytes))
	}
	var l cispclient.UsspList
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return nil, refuse(USSPList, 0, "body: not a cis/ussp_list/v1 document: "+short(err.Error()))
	}
	if dec.More() {
		return nil, refuse(USSPList, 0, "body: trailing data after the document")
	}
	if l.Schema != "cis/ussp_list/v1" {
		return nil, refuse(USSPList, 0, fmt.Sprintf("schema: %q is not cis/ussp_list/v1", short(string(l.Schema))))
	}
	if l.CisDataset == nil || string(*l.CisDataset) != string(USSPList) {
		return nil, refuse(USSPList, 0, "cis_dataset: missing or not ussp_list")
	}
	if l.CisVersion == nil || *l.CisVersion < 1 {
		return nil, refuse(USSPList, 0, "cis_version: missing or not an integer of at least 1")
	}
	if headerVersion > 0 && headerVersion != *l.CisVersion {
		return nil, refuse(USSPList, *l.CisVersion, fmt.Sprintf("cis_version: %d is not X-CIS-Version %d", *l.CisVersion, headerVersion))
	}
	issued := l.Issued.UTC()
	meta := Metadata{CISDataset: string(USSPList), CISVersion: *l.CisVersion, Issued: &issued, USSPList: bytes.Clone(body)}
	if l.CisUpdatedAt != nil {
		t := l.CisUpdatedAt.UTC()
		meta.CISUpdatedAt = &t
	}
	return &Version{Dataset: USSPList, Number: *l.CisVersion, ETag: etag, Body: body, USSPList: &l, Meta: meta}, nil
}

// short bounds an untrusted string quoted in a problem.
func short(s string) string {
	const maxRunes = 80
	if len(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "..."
}

// versionLabel is "<dataset>:<version>".
func versionLabel(d Dataset, v int64) string { return string(d) + ":" + strconv.FormatInt(v, 10) }

// joinLabels joins labels with commas.
func joinLabels(ls []string) string { return strings.Join(ls, ",") }
