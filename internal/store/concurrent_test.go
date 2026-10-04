package store

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// indexRuleFrom is the first relational migration held to the rule of
// TestIndexesOnLiveTablesAreBuiltConcurrently (the rule arrived with the
// review of WP-15; earlier files are applied everywhere already).
const indexRuleFrom = 20

// notLiveUntil names the tables nothing wrote before a migration that
// indexes them in its transaction, and the last such migration: 00005
// created occurrence_reports and operating_status_notices and nothing
// wrote them before 00020 and 00021 (each file's header says so).
var notLiveUntil = map[string]int{"occurrence_reports": 20, "operating_status_notices": 21}

var createIndex = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+(CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?[a-z0-9_]+\s+ON\s+(?:ONLY\s+)?"?([a-z_][a-z0-9_]*)"?`)

// liveIndexFaults lists every index the Up section of migration name
// (version v) builds on a table an earlier migration created without
// CREATE INDEX CONCURRENTLY in a -- +goose NO TRANSACTION file: a plain
// CREATE INDEX holds a SHARE lock that stops every write to the table
// for the whole build.
func liveIndexFaults(name string, v int, src string) []string {
	up, _, _ := strings.Cut(src, "-- +goose Down")
	noTx := strings.Contains(src, "-- +goose NO TRANSACTION")
	code := sqlCode(up)
	created := map[string]bool{}
	for _, m := range createTable.FindAllStringSubmatch(code, -1) {
		created[m[1]] = true
	}
	var out []string
	for _, m := range createIndex.FindAllStringSubmatch(code, -1) {
		table := m[2]
		if created[table] || v <= notLiveUntil[table] {
			continue
		}
		if m[1] == "" || !noTx {
			out = append(out, name+": "+table)
		}
	}
	return out
}

// An index on a table that is live by then is built concurrently,
// outside a transaction.
func TestIndexesOnLiveTablesAreBuiltConcurrently(t *testing.T) {
	n := 0
	for name, src := range files(t, treeFS(t, TreeRelational)) {
		digits, _, _ := strings.Cut(name, "_")
		v, err := strconv.Atoi(digits)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v < indexRuleFrom {
			continue
		}
		n++
		for _, f := range liveIndexFaults(name, v, src) {
			t.Errorf("an index built in a transaction on a live table: %s", f)
		}
	}
	if n == 0 {
		t.Fatal("no migration checked")
	}
}

// E-01: the rule flags a plain index and a concurrent one in a
// transaction, and takes a concurrent one outside a transaction, an
// index on a table created in the same file, and one on a table listed
// as not live yet.
func TestLiveIndexRuleCatchesAPlainIndex(t *testing.T) {
	const tail = "\n-- +goose Down\nDROP INDEX x;\n"
	for name, c := range map[string]struct {
		src  string
		v    int
		want int
	}{
		"plain":           {"-- +goose Up\nCREATE INDEX a_idx ON alerts (kind);" + tail, 30, 1},
		"unique plain":    {"-- +goose Up\nCREATE UNIQUE INDEX a_idx ON alerts (kind);" + tail, 30, 1},
		"concurrent, tx":  {"-- +goose Up\nCREATE INDEX CONCURRENTLY a_idx ON alerts (kind);" + tail, 30, 1},
		"concurrent":      {"-- +goose NO TRANSACTION\n-- +goose Up\nCREATE INDEX CONCURRENTLY IF NOT EXISTS a_idx ON alerts (kind);" + tail, 30, 0},
		"own table":       {"-- +goose Up\nCREATE TABLE fresh (id uuid);\nCREATE INDEX f_idx ON fresh (id);" + tail, 30, 0},
		"not live yet":    {"-- +goose Up\nCREATE INDEX o_idx ON occurrence_reports (id);" + tail, 20, 0},
		"live after all":  {"-- +goose Up\nCREATE INDEX o_idx ON occurrence_reports (id);" + tail, 22, 1},
		"down is not run": {"-- +goose Up\nSELECT 1;\n-- +goose Down\nCREATE INDEX a_idx ON alerts (kind);\n", 30, 0},
	} {
		if got := liveIndexFaults("x.sql", c.v, c.src); len(got) != c.want {
			t.Errorf("%s: %v, want %d faults", name, got, c.want)
		}
	}
}
