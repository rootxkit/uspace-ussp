package testif

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rootxkit/uspace-ussp/internal/config"
)

func env(m map[string]string) config.LookupFunc {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// The gate refuses without the switch, without a test or development
// system id, and with neither, naming both variables each time; it
// admits the two together (E-01).
func TestGateRefusesUnlessSwitchAndTestSystemID(t *testing.T) {
	refused := []map[string]string{
		{},
		{VarSwitch: "off", VarSystemID: "USSP-DEV"},
		{VarSwitch: "ON", VarSystemID: "USSP-DEV"},
		{VarSwitch: "on"},
		{VarSwitch: "on", VarSystemID: ""},
		{VarSwitch: "on", VarSystemID: "GEOUSSP1"},
		{VarSwitch: "on", VarSystemID: "ussp-dev"},
		{VarSystemID: "LABTEST1"},
	}
	for _, m := range refused {
		err := Gate(env(m))
		if !errors.Is(err, ErrRefused) {
			t.Errorf("%v: %v, want refused", m, err)
			continue
		}
		if !strings.Contains(err.Error(), VarSwitch) || !strings.Contains(err.Error(), VarSystemID) {
			t.Errorf("%v: %q does not name both variables", m, err)
		}
	}
	for _, id := range []string{"USSP-DEV", "LABTEST1", "TEST", "DEV01"} {
		if err := Gate(env(map[string]string{VarSwitch: "on", VarSystemID: id})); err != nil {
			t.Errorf("%s: %v, want admitted", id, err)
		}
	}
}

func validEnv() map[string]string {
	return map[string]string{
		"USSP_TEST_INTERFACES_USSP_BASE_URL":        "https://ussp.uspace.test/",
		"USSP_TEST_INTERFACES_OPERATOR_CLIENT_ID":   "op-test",
		"USSP_TEST_INTERFACES_OPERATOR_SECRET_FILE": "/run/secrets/op",
		"USSP_TEST_INTERFACES_OPERATOR_FILE":        "/cfg/test-operator.yaml",
	}
}

// A complete configuration loads with the defaults; each required
// variable missing, and each malformed value, is named.
func TestLoad(t *testing.T) {
	c, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8087" || c.USSPBaseURL != "https://ussp.uspace.test" || c.MaxTests != 16 || c.AckTimeoutS != 15 {
		t.Errorf("%+v", c)
	}
	for _, name := range []string{"USSP_TEST_INTERFACES_USSP_BASE_URL", "USSP_TEST_INTERFACES_OPERATOR_CLIENT_ID",
		"USSP_TEST_INTERFACES_OPERATOR_SECRET_FILE", "USSP_TEST_INTERFACES_OPERATOR_FILE"} {
		m := validEnv()
		delete(m, name)
		if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: %v", name, err)
		}
	}
	for name, bad := range map[string]string{
		"USSP_TEST_INTERFACES_USSP_BASE_URL": "https://user:pw@ussp.test",
		"USSP_TEST_INTERFACES_MAX_TESTS":     "0",
		"USSP_TEST_INTERFACES_ACK_TIMEOUT_S": "x",
	} {
		m := validEnv()
		m[name] = bad
		_, err := Load(env(m))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%s: %v", name, bad, err)
		}
		if strings.Contains(err.Error(), "pw@") {
			t.Errorf("%s: the error echoes a credential: %v", name, err)
		}
	}
}

// deploy/ENV.md's test-interfaces table lists exactly Variables, with
// their requirement, default and unit.
func TestEnvMarkdownDocumentsTheVariables(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "deploy", "ENV.md"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows := map[string][]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "| `"+Process+"` |") {
			continue
		}
		cells := strings.Split(line, "|")
		for i := range cells {
			cells[i] = strings.Trim(strings.TrimSpace(cells[i]), "`")
		}
		rows[cells[2]] = cells[3:]
	}
	for _, v := range Variables() {
		row, ok := rows[v.Name]
		if !ok {
			t.Errorf("%s is not in deploy/ENV.md's test-interfaces table", v.Name)
			continue
		}
		delete(rows, v.Name)
		req := ""
		if v.Required {
			req = Process
		}
		if row[0] != req || row[1] != v.Default || row[2] != v.Unit {
			t.Errorf("%s: ENV.md says required %q, default %q, unit %q; the code %q, %q, %q", v.Name, row[0], row[1], row[2], req, v.Default, v.Unit)
		}
	}
	for name := range rows {
		t.Errorf("deploy/ENV.md documents %s, which test-interfaces does not read", name)
	}
}
