package store

import (
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// sqlCode is a migration file without its comments and string
// literals: what the database executes, not what the prose mentions.
func sqlCode(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return regexp.MustCompile(`'[^']*'`).ReplaceAllString(b.String(), "''")
}

var createTable = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?`)

// tables returns every table a tree creates.
func tables(t *testing.T, fsys fs.FS) []string {
	t.Helper()
	var out []string
	for _, src := range files(t, fsys) {
		// The partitions events_ensure_partition creates at run time are
		// in a string literal, which sqlCode strips.
		for _, m := range createTable.FindAllStringSubmatch(sqlCode(src), -1) {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

func files(t *testing.T, fsys fs.FS) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".sql") {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		out[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// misplaced lists "file: table" for every file of fsys whose SQL names a
// table that only the other tree creates.
func misplaced(t *testing.T, fsys fs.FS, foreign []string) []string {
	t.Helper()
	var out []string
	for name, src := range files(t, fsys) {
		code := sqlCode(src)
		for _, table := range foreign {
			if regexp.MustCompile(`\b` + table + `\b`).MatchString(code) {
				out = append(out, name+": "+table)
			}
		}
	}
	slices.Sort(out)
	return out
}

func treeFS(t *testing.T, tree Tree) fs.FS {
	t.Helper()
	f, err := tree.Files()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// No file of one tree names a table of the other (CLAUDE.md rule 10):
// a migration placed in the wrong tree is caught here, before it runs
// against the wrong database.
func TestNoTreeNamesTheOtherTreesTables(t *testing.T) {
	rel, ts := tables(t, treeFS(t, TreeRelational)), tables(t, treeFS(t, TreeTimeseries))
	if len(rel) < 25 || len(ts) != 7 {
		t.Fatalf("tables: relational %d %v, timeseries %d %v", len(rel), rel, len(ts), ts)
	}
	for _, name := range rel {
		if slices.Contains(ts, name) {
			t.Errorf("table %s is created by both trees", name)
		}
	}
	if bad := misplaced(t, treeFS(t, TreeRelational), ts); len(bad) > 0 {
		t.Errorf("relational files name time-series tables: %v", bad)
	}
	if bad := misplaced(t, treeFS(t, TreeTimeseries), rel); len(bad) > 0 {
		t.Errorf("time-series files name relational tables: %v", bad)
	}
}

// E-01 twin: the check finds a misplaced migration. A hypertable
// migration dropped into the relational tree, and an alerts change
// dropped into the time-series tree, are both named.
func TestMisplacedMigrationIsCaught(t *testing.T) {
	rel := fstest.MapFS{
		"00001_ok.sql":    {Data: []byte("-- mentions telemetry in prose only\nCREATE TABLE flights (id uuid, note text DEFAULT 'telemetry');")},
		"00008_wrong.sql": {Data: []byte("SELECT add_retention_policy('peer_flights', INTERVAL '1 day');\nALTER TABLE telemetry ADD COLUMN x int;")},
	}
	if got := misplaced(t, rel, []string{"telemetry", "peer_flights"}); strings.Join(got, ",") != "00008_wrong.sql: telemetry" {
		t.Errorf("relational: %v", got)
	}
	ts := fstest.MapFS{"00003_wrong.sql": {Data: []byte("ALTER TABLE alerts ADD COLUMN y int;")}}
	if got := misplaced(t, ts, []string{"alerts"}); strings.Join(got, ",") != "00003_wrong.sql: alerts" {
		t.Errorf("timeseries: %v", got)
	}
}

// Every file has an Up and a Down section (the trees go up, down, up).
func TestEveryMigrationHasUpAndDown(t *testing.T) {
	for _, tree := range Trees() {
		for name, src := range files(t, treeFS(t, tree)) {
			up, down := strings.Index(src, "-- +goose Up"), strings.Index(src, "-- +goose Down")
			if up < 0 || down < up {
				t.Errorf("%s/%s: want -- +goose Up then -- +goose Down", tree, name)
			}
		}
	}
}

var columnLine = regexp.MustCompile(`(?m)^\s+"?([a-z_][a-z0-9_]*)"?\s+(?:text|bigint|integer|boolean|double precision|jsonb|uuid|timestamptz|date|bigserial|geography|geometry|text\[\])`)

// badColumns are column names without a unit or datum, or a stored AGL
// (CLAUDE.md rule 11, D-02).
func badColumns(src string) []string {
	var out []string
	for _, m := range columnLine.FindAllStringSubmatch(sqlCode(src), -1) {
		c := m[1]
		switch {
		case c == "alt" || c == "height" || c == "dist" || c == "speed" || c == "altitude":
			out = append(out, c+": no unit or datum")
		case strings.Contains(c, "agl"):
			out = append(out, c+": stored AGL")
		case strings.HasPrefix(c, "alt_") && !strings.HasSuffix(c, "_m"):
			out = append(out, c+": altitude without metres")
		}
	}
	return out
}

func TestColumnsCarryUnitsAndNoAGL(t *testing.T) {
	n := 0
	for _, tree := range Trees() {
		for name, src := range files(t, treeFS(t, tree)) {
			n += len(columnLine.FindAllString(sqlCode(src), -1))
			if bad := badColumns(src); len(bad) > 0 {
				t.Errorf("%s/%s: %v", tree, name, bad)
			}
		}
	}
	if n < 200 {
		t.Fatalf("only %d columns parsed: the column pattern no longer matches the files", n)
	}
	// E-01 twin: the check flags what it exists to flag.
	got := badColumns("CREATE TABLE x (\n    alt double precision,\n    height_agl_m double precision,\n    alt_amsl double precision,\n    alt_amsl_m double precision\n);")
	if strings.Join(got, ";") != "alt: no unit or datum;height_agl_m: stored AGL;alt_amsl: altitude without metres" {
		t.Errorf("badColumns: %v", got)
	}
}

func TestLatestAndVersionTables(t *testing.T) {
	for _, tree := range Trees() {
		v, err := Latest(tree)
		if err != nil || v < 2 {
			t.Errorf("%s: latest %d %v", tree, v, err)
		}
	}
	if TreeRelational.VersionTable() == TreeTimeseries.VersionTable() || TreeRelational.lockID() == TreeTimeseries.lockID() {
		t.Error("the trees share a version table or a lock")
	}
	if TreeRelational.VersionTable() != "goose_db_version_relational" || TreeTimeseries.VersionTable() != "goose_db_version_timeseries" {
		t.Error("version table names differ from docs/PLAN.md D5")
	}
	if _, err := ParseTree("merged"); err == nil {
		t.Error("ParseTree accepted merged")
	}
	for _, tree := range Trees() {
		if got, err := ParseTree(string(tree)); err != nil || got != tree {
			t.Errorf("ParseTree(%s) = %s %v", tree, got, err)
		}
	}
	if _, err := Latest(Tree("merged")); err == nil {
		t.Error("Latest of an unknown tree")
	}
}

func TestVersionErrorNamesBothVersionsAndTheCommand(t *testing.T) {
	e := &VersionError{Tree: TreeTimeseries, Have: 1, Want: 2}
	if e.Error() != "timeseries schema is at version 1, this build needs 2: run `ussp-tsdb-writer migrate`" {
		t.Error(e.Error())
	}
	e = &VersionError{Tree: TreeRelational, Have: 0, Want: 7}
	if !strings.Contains(e.Error(), "version 0, this build needs 7: run `ussp-api migrate`") {
		t.Error(e.Error())
	}
}

func TestOpenPoolRefusesABadURLOrRoleWithoutEchoingIt(t *testing.T) {
	if _, err := OpenPool(PoolOptions{URL: "postgres://user:hunter2@[::1"}); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("bad URL: %v", err)
	}
	if _, err := OpenPool(PoolOptions{URL: "postgres://u@127.0.0.1/db", Role: "x; DROP"}); err == nil {
		t.Error("bad role accepted")
	}
	p, err := OpenPool(PoolOptions{URL: "postgres://u@127.0.0.1/db", Role: AppRole, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	s, err := Open(t.Context(), Config{TSURL: "postgres://u@127.0.0.1/ts"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Rel != nil || s.TS == nil {
		t.Fatalf("pools %v %v", s.Rel, s.TS)
	}
	if _, err := s.Pool(TreeRelational); err == nil {
		t.Error("relational pool of a store without one")
	}
	if err := s.Tx(t.Context(), nil); err == nil {
		t.Error("Tx without a relational pool")
	}
}
