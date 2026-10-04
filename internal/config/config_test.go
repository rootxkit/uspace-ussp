package config

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func env(kv map[string]string) LookupFunc {
	return func(name string) (string, bool) {
		v, ok := kv[name]
		return v, ok
	}
}

func fieldNames(err error) []string {
	var out []string
	for _, fe := range FieldErrors(err) {
		out = append(out, fe.Field)
	}
	return out
}

func TestDefaultsLoadFromAnEmptyEnvironment(t *testing.T) {
	c, err := LoadFrom(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "info" || c.MaxBodyBytes != 1<<20 || c.ShutdownTimeoutS != 15 || c.APIAddr != ":8080" ||
		c.TSDBWriterAddr != ":8086" || c.MTLSMode != "required" || c.AuthorityPush != "off" || c.SystemID != "USSP-DEV" {
		t.Fatalf("defaults: %+v", c)
	}
}

// Each process names exactly the variables it needs (E-01: the refusal
// beside the configuration it accepts).
func TestRequireNamesWhatEachProcessNeeds(t *testing.T) {
	c, err := LoadFrom(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	for proc, want := range map[string][]string{
		ProcessAPI:             {"USSP_PG_URL", "USSP_TS_URL", "USSP_NATS_URL"},
		ProcessTSDBWriter:      {"USSP_TS_URL", "USSP_NATS_URL"},
		ProcessTelemetryIngest: {"USSP_NATS_URL"},
		ProcessRIDSP:           {"USSP_NATS_URL"},
		ProcessMonitor:         {"USSP_NATS_URL"},
		ProcessTrafficWS:       {"USSP_NATS_URL"},
		ProcessDSSSync:         {"USSP_NATS_URL"},
	} {
		if got := fieldNames(c.Require(proc)); !slices.Equal(got, want) {
			t.Errorf("%s: missing %v, want %v", proc, got, want)
		}
	}
	c, err = LoadFrom(env(map[string]string{
		"USSP_PG_URL":   "postgres://ussp:pw@db:5432/ussp_relational",
		"USSP_TS_URL":   "postgres://ussp:pw@db:5432/ussp_timeseries",
		"USSP_NATS_URL": "nats://nats:4222",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Require(Processes...); err != nil {
		t.Fatalf("a complete configuration was refused: %v", err)
	}
}

func TestInvalidValuesAreAllNamedInOneRun(t *testing.T) {
	_, err := LoadFrom(env(map[string]string{
		"USSP_LOG_LEVEL":           "loud",
		"USSP_SHUTDOWN_TIMEOUT_S":  "0",
		"USSP_HTTP_MAX_BODY_BYTES": "lots",
		"USSP_NATS_URL":            "localhost",
		"USSP_TOKEN_ISSUERS":       "https://issuer.example",
		"USSP_MTLS_MODE":           "maybe",
	}))
	want := []string{"USSP_LOG_LEVEL", "USSP_SHUTDOWN_TIMEOUT_S", "USSP_HTTP_MAX_BODY_BYTES", "USSP_NATS_URL", "USSP_TOKEN_ISSUERS", "USSP_MTLS_MODE"}
	if got := fieldNames(err); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTwoTreesNeedTwoDatabases(t *testing.T) {
	_, err := LoadFrom(env(map[string]string{
		"USSP_PG_URL": "postgres://u:p@db/one",
		"USSP_TS_URL": "postgres://u:p@db/one",
	}))
	if got := fieldNames(err); !slices.Equal(got, []string{"USSP_TS_URL"}) {
		t.Fatalf("got %v", got)
	}
}

func TestListenAddressesMustDiffer(t *testing.T) {
	_, err := LoadFrom(env(map[string]string{"USSP_MONITOR_ADDR": ":8080"}))
	if got := fieldNames(err); !slices.Equal(got, []string{"USSP_MONITOR_ADDR"}) {
		t.Fatalf("got %v", got)
	}
	if _, err := LoadFrom(env(map[string]string{"USSP_API_ADDR": "127.0.0.1:0", "USSP_MONITOR_ADDR": "127.0.0.1:0"})); err != nil {
		t.Fatalf("two :0 addresses refused: %v", err)
	}
}

func TestRedactedRemovesCredentials(t *testing.T) {
	c, err := LoadFrom(env(map[string]string{
		"USSP_PG_URL":   "postgres://ussp_api:hunter2@db:5432/ussp_relational",
		"USSP_TS_URL":   "postgres://ussp_ts:hunter3@db:5432/ussp_timeseries",
		"USSP_NATS_URL": "nats://s3cr3t-token@nats:4222",
	}))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Redacted()
	for _, secret := range []string{"hunter2", "hunter3", "s3cr3t-token"} {
		if strings.Contains(s, secret) {
			t.Errorf("%q in %s", secret, s)
		}
	}
	for _, kept := range []string{`USSP_PG_URL="postgres://ussp_api:xxxxx@db:5432/ussp_relational"`, `USSP_NATS_URL="nats://xxxxx@nats:4222"`, "USSP_API_ADDR="} {
		if !strings.Contains(s, kept) {
			t.Errorf("%q missing from %s", kept, s)
		}
	}
}

func TestParseIssuers(t *testing.T) {
	got, err := ParseIssuers([]string{"https://auth.example/=https://auth.example/.well-known/jwks.json?a=b"})
	if err != nil || len(got) != 1 || got[0].Issuer != "https://auth.example/" || got[0].JWKSURL != "https://auth.example/.well-known/jwks.json?a=b" {
		t.Fatalf("got %+v %v", got, err)
	}
	for _, bad := range []string{"https://auth.example", "=https://x/jwks", "iss=", "iss=relative/jwks"} {
		if _, err := ParseIssuers([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAddrAndHelp(t *testing.T) {
	c, _ := LoadFrom(env(nil))
	for _, p := range Processes {
		if c.Addr(p) == "" {
			t.Errorf("%s has no address", p)
		}
	}
	if c.Addr("nope") != "" {
		t.Error("an unknown process has an address")
	}
	h := Help(ProcessTSDBWriter)
	if !strings.Contains(h, "USSP_TS_URL (required)") || !strings.Contains(h, "USSP_TSDB_WRITER_ADDR (default :8086)") ||
		strings.Contains(h, "USSP_PG_URL") {
		t.Fatalf("help:\n%s", h)
	}
}

// deploy/ENV.md documents every variable with the processes that read
// it and its default, and nothing else.
func TestEnvMarkdownDocumentsEveryVariable(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "deploy", "ENV.md"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	documented := map[string][]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "| `USSP_") {
			continue
		}
		cells := strings.Split(line, "|")
		for i := range cells {
			cells[i] = strings.Trim(strings.TrimSpace(cells[i]), "`")
		}
		documented[cells[1]] = cells[2:]
	}
	for _, v := range Variables() {
		row, ok := documented[v.Name]
		if !ok {
			t.Errorf("%s is not documented in deploy/ENV.md", v.Name)
			continue
		}
		delete(documented, v.Name)
		if row[0] != v.ReadBy || row[1] != v.Required || row[2] != v.Default || row[3] != v.Unit {
			t.Errorf("%s: ENV.md says read by %q, required by %q, default %q, unit %q; the code says %q, %q, %q, %q",
				v.Name, row[0], row[1], row[2], row[3], v.ReadBy, v.Required, v.Default, v.Unit)
		}
	}
	for name := range documented {
		t.Errorf("deploy/ENV.md documents %s, which no process reads", name)
	}
}

// USSP_CERTIFICATE_ID is the authority's 32-hex certificate id or unset
// (E-01 pair: a well-formed id loads).
func TestCertificateID(t *testing.T) {
	ok := map[string]string{"USSP_CERTIFICATE_ID": "0123456789abcdef0123456789abcdef"}
	if c, err := LoadFrom(env(ok)); err != nil || c.CertificateID != ok["USSP_CERTIFICATE_ID"] {
		t.Fatalf("%v %q", err, c.CertificateID)
	}
	for _, bad := range []string{"0123456789ABCDEF0123456789ABCDEF", "123", "0123456789abcdef0123456789abcdeg"} {
		if _, err := LoadFrom(env(map[string]string{"USSP_CERTIFICATE_ID": bad})); err == nil || !strings.Contains(err.Error(), "USSP_CERTIFICATE_ID") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
