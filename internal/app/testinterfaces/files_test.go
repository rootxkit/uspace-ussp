package testinterfaces

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

func repoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stages splits a Dockerfile into its stages by FROM line, with the
// name each is given (AS name; "" without one).
func stages(t *testing.T, dockerfile string) (names []string, bodies []string) {
	t.Helper()
	from := regexp.MustCompile(`(?m)^FROM\s+\S+(?:\s+AS\s+(\S+))?\s*$`)
	idx := from.FindAllStringSubmatchIndex(dockerfile, -1)
	if len(idx) < 2 {
		t.Fatalf("%d stages", len(idx))
	}
	for i, m := range idx {
		end := len(dockerfile)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		name := ""
		if m[2] >= 0 {
			name = dockerfile[m[2]:m[3]]
		}
		names = append(names, name)
		bodies = append(bodies, dockerfile[m[0]:end])
	}
	return names, bodies
}

// The production image (the Dockerfile's last stage, the default
// target) neither builds nor copies ussp-test-interfaces; the testif
// target is the separate image whose entrypoint it is (D13). The image
// job checks the built image the same way (the hook: `docker run
// --entrypoint /usr/bin/ussp-test-interfaces` on the image under test
// must fail).
func TestProductionImageHasNoTestInterfaces(t *testing.T) {
	names, bodies := stages(t, repoFile(t, "deploy/Dockerfile"))
	last := bodies[len(bodies)-1]
	if names[len(names)-1] != "production" || strings.Contains(last, "test-interfaces") || strings.Contains(last, "testif") {
		t.Fatalf("the last stage %q is not the production stage without test-interfaces:\n%s", names[len(names)-1], last)
	}
	var build, testif string
	for i, n := range names {
		switch n {
		case "build":
			build = bodies[i]
		case "testif":
			testif = bodies[i]
		}
	}
	loop := regexp.MustCompile(`for p in ([^;]+);`).FindStringSubmatch(build)
	if len(loop) < 2 || strings.Contains(loop[1], "test-interfaces") || !strings.Contains(loop[1], "api") {
		t.Errorf("the production build loop %v names test-interfaces, or is not the loop", loop)
	}
	if !strings.Contains(last, "COPY --from=build /out/ /usr/bin/") {
		t.Error("the production stage does not copy the production build")
	}
	if !strings.Contains(testif, `ENTRYPOINT ["/usr/bin/`+Binary+`"]`) || strings.Contains(testif, "--from=build ") {
		t.Errorf("the testif stage is not test-interfaces alone:\n%s", testif)
	}
}

// The conformance overlay runs test-interfaces from the testif image
// with the switch on, only when named (its own profile), and routes the
// lab Caddy's /test/* to it through a Caddyfile the hook writes; the
// production compose has none of it (TestProductionComposeHasNoTestInterfaces).
func TestConformanceOverlayRunsTestInterfaces(t *testing.T) {
	var doc struct {
		Services map[string]struct {
			Image       string            `yaml:"image"`
			Profiles    []string          `yaml:"profiles"`
			Entrypoint  []string          `yaml:"entrypoint"`
			Environment map[string]string `yaml:"environment"`
			Volumes     []string          `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(repoFile(t, "deploy/conformance/compose.yaml")), &doc); err != nil {
		t.Fatal(err)
	}
	s, ok := doc.Services["ussp-test-interfaces"]
	if !ok {
		t.Fatal("the overlay defines no ussp-test-interfaces")
	}
	if !strings.Contains(s.Image, "USSP_TESTIF_IMAGE") || len(s.Profiles) != 1 || s.Profiles[0] != "testif" ||
		len(s.Entrypoint) != 1 || s.Entrypoint[0] != "/usr/bin/"+Binary {
		t.Errorf("%+v", s)
	}
	if s.Environment["USSP_TEST_INTERFACES"] != "on" || !strings.Contains(s.Environment["USSP_SYSTEM_ID"], "USSP_SYSTEM_ID") {
		t.Errorf("environment %v", s.Environment)
	}
	for k, v := range s.Environment {
		if strings.Contains(k, "SECRET") && !strings.HasSuffix(k, "_FILE") {
			t.Errorf("%s=%s: a secret by value", k, v)
		}
	}
	caddy := doc.Services["caddy"]
	if len(caddy.Volumes) != 1 || !strings.HasSuffix(caddy.Volumes[0], ":/etc/caddy/Caddyfile:ro") {
		t.Errorf("the lab Caddy's Caddyfile is not the hook's: %v", caddy.Volumes)
	}
	hook := repoFile(t, "scripts/conformance.sh")
	for _, want := range []string{"@testif path /test/*", "reverse_proxy ussp-test-interfaces:8087"} {
		if !strings.Contains(hook, want) {
			t.Errorf("the hook's Caddyfile has no %q", want)
		}
	}
}
