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
// created occurrence_reports, operating_status_notices and
// weather_products and nothing wrote them before 00020, 00021 and 00024
// (each file's header says so).
var notLiveUntil = map[string]int{"occurrence_reports": 20, "operating_status_notices": 21, "weather_products": 24}

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

// checkRuleFrom is the first relational migration held to the rule of
// TestChecksOnLiveTablesAreAddedNotValid (the rule arrived with the
// review of WP-18; earlier files are applied everywhere already).
const checkRuleFrom = 25

var (
	alterTable    = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?"?([a-z_][a-z0-9_]*)"?\s+(.*)$`)
	checkClause   = regexp.MustCompile(`(?i)\bCHECK\s*\(`)
	notValid      = regexp.MustCompile(`(?i)\bNOT\s+VALID\s*$`)
	addNamed      = regexp.MustCompile(`(?i)\bADD\s+CONSTRAINT\s+"?([a-z_][a-z0-9_]*)"?`)
	validateNamed = regexp.MustCompile(`(?i)\bVALIDATE\s+CONSTRAINT\s+"?([a-z_][a-z0-9_]*)"?`)
)

// clauses splits the actions of an ALTER TABLE at its top-level commas.
func clauses(actions string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range actions {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(actions[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(actions[start:]))
}

// liveCheckFaults lists every CHECK the Up section of migration name
// adds to a table an earlier migration created without NOT VALID (a
// CHECK added valid scans the whole table under the ACCESS EXCLUSIVE
// lock of ALTER TABLE, stopping every read and write of it; a column
// CHECK cannot be NOT VALID, so it is written as a table constraint),
// and every constraint the same file both adds and validates (the
// VALIDATE belongs to a later step, where it holds only a SHARE UPDATE
// EXCLUSIVE lock).
func liveCheckFaults(name, src string) []string {
	up, _, _ := strings.Cut(src, "-- +goose Down")
	code := sqlCode(up)
	created := map[string]bool{}
	for _, m := range createTable.FindAllStringSubmatch(code, -1) {
		created[m[1]] = true
	}
	var out []string
	added := map[string]bool{}
	for _, stmt := range strings.Split(code, ";") {
		m := alterTable.FindStringSubmatch(stmt)
		if m == nil {
			continue
		}
		for _, c := range clauses(m[2]) {
			if n := addNamed.FindStringSubmatch(c); n != nil {
				added[n[1]] = true
			}
			if !created[m[1]] && checkClause.MatchString(c) && !notValid.MatchString(c) {
				out = append(out, name+": a CHECK added valid to "+m[1])
			}
		}
	}
	for _, m := range validateNamed.FindAllStringSubmatch(code, -1) {
		if added[m[1]] {
			out = append(out, name+": "+m[1]+" added and validated in one step")
		}
	}
	return out
}

// A CHECK on a table that is live by then is added NOT VALID and
// validated in a later migration.
func TestChecksOnLiveTablesAreAddedNotValid(t *testing.T) {
	n := 0
	for name, src := range files(t, treeFS(t, TreeRelational)) {
		digits, _, _ := strings.Cut(name, "_")
		v, err := strconv.Atoi(digits)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v < checkRuleFrom {
			continue
		}
		n++
		for _, f := range liveCheckFaults(name, src) {
			t.Errorf("%s", f)
		}
	}
	if n == 0 {
		t.Fatal("no migration checked")
	}
}

// E-01: the rule flags a column CHECK, a table CHECK added valid and a
// constraint validated where it is added, and takes a NOT VALID one, a
// CHECK on a table the file creates, and a VALIDATE of an earlier
// file's constraint.
func TestLiveCheckRuleCatchesAValidCheck(t *testing.T) {
	const tail = "\n-- +goose Down\nALTER TABLE alerts DROP CONSTRAINT c;\n"
	for name, c := range map[string]struct {
		src  string
		want int
	}{
		"column check":      {"-- +goose Up\nALTER TABLE alerts ADD COLUMN n bigint NOT NULL DEFAULT 1 CHECK (n >= 1);" + tail, 1},
		"table check":       {"-- +goose Up\nALTER TABLE alerts ADD COLUMN n bigint, ADD CONSTRAINT c CHECK (n IS NULL OR n > 0);" + tail, 1},
		"one of two":        {"-- +goose Up\nALTER TABLE alerts ADD CONSTRAINT a CHECK (x > 0) NOT VALID, ADD CONSTRAINT b CHECK (y > (0));" + tail, 1},
		"not valid":         {"-- +goose Up\nALTER TABLE alerts ADD COLUMN n bigint, ADD CONSTRAINT c CHECK ((n IS NULL) = (m IS NULL)) NOT VALID;" + tail, 0},
		"own table":         {"-- +goose Up\nCREATE TABLE fresh (n int);\nALTER TABLE fresh ADD CONSTRAINT c CHECK (n > 0);" + tail, 0},
		"validated at once": {"-- +goose Up\nALTER TABLE alerts ADD CONSTRAINT c CHECK (n > 0) NOT VALID;\nALTER TABLE alerts VALIDATE CONSTRAINT c;" + tail, 1},
		"validated later":   {"-- +goose NO TRANSACTION\n-- +goose Up\nALTER TABLE alerts VALIDATE CONSTRAINT c;" + tail, 0},
		"down is not run":   {"-- +goose Up\nSELECT 1;\n-- +goose Down\nALTER TABLE alerts ADD CONSTRAINT c CHECK (n > 0);\n", 0},
	} {
		if got := liveCheckFaults("x.sql", c.src); len(got) != c.want {
			t.Errorf("%s: %v, want %d faults", name, got, c.want)
		}
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
