package national

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// bffPaths reads the upstream paths the portal's BFF lets through
// (web/src/lib/bff/paths.ts, allowPaths) with every parameter segment
// (a ${...} or a [...] pattern) written {}.
func bffPaths(t *testing.T) []string {
	t.Helper()
	return bffPathsOf(t, "allowPaths")
}

// bffPathsOf reads the paths the function fn of paths.ts returns.
func bffPathsOf(t *testing.T, fn string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "bff", "paths.ts"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "export function "+fn+"(")
	if start < 0 {
		t.Fatalf("paths.ts has no %s", fn)
	}
	src = src[start:]
	open, end := strings.Index(src, "return ["), strings.Index(src, "].map(")
	if open < 0 || end < open {
		t.Fatal("allowPaths returns no array literal")
	}
	var out []string
	for _, m := range regexp.MustCompile("[\"`](/[^\"`]*)[\"`]").FindAllStringSubmatch(src[open:end], -1) {
		segs := strings.Split(m[1], "/")
		for i, s := range segs {
			if strings.ContainsAny(s, "$[") {
				segs[i] = "{}"
			}
		}
		out = append(out, strings.Join(segs, "/"))
	}
	return out
}

// routePaths are the paths of AccessTable with every {name} written {}.
func routePaths() map[string]bool {
	param := regexp.MustCompile(`\{[^}]+\}`)
	out := map[string]bool{}
	for pattern := range AccessTable() {
		_, path, _ := strings.Cut(pattern, " ")
		out[param.ReplaceAllString(path, "{}")] = true
	}
	return out
}

// The BFF proxies to this process (USSP_WEB_API_URL is api), so every
// path it lets through is one api serves: a path served by another
// process (traffic-ws's /v1/traffic/snapshot) is a 404 here whatever the
// allow-list says. Twin: the list is read (not empty) and carries the
// portal's own operations.
func TestBFFAllowListIsAPIRoutes(t *testing.T) {
	paths, routes := bffPaths(t), routePaths()
	if len(paths) < 10 {
		t.Fatalf("read %d BFF paths: %v", len(paths), paths)
	}
	for _, want := range []string{"/v1/intents/{}", "/v1/alerts/{}/ack", "/v1/accounts/operators/{}/clients/{}/serials/{}"} {
		if !slices.Contains(paths, want) {
			t.Errorf("the BFF paths %v do not carry %s", paths, want)
		}
	}
	for _, p := range paths {
		if !routes[p] {
			t.Errorf("the BFF lets %s through, which api does not serve", p)
		}
	}
}

// The console's BFF (brief WP-18) lets through only api's routes too,
// the console's operations among them, and none of the portal's
// writes. Twin: the list is read and carries the console's own.
func TestConsoleBFFAllowListIsAPIRoutes(t *testing.T) {
	paths, routes := bffPathsOf(t, "consoleAllowPaths"), routePaths()
	if len(paths) < 10 {
		t.Fatalf("read %d console BFF paths: %v", len(paths), paths)
	}
	for _, want := range []string{"/v1/admin/inputs", "/v1/admin/alerts/{}/escalate", "/v1/admin/emergency/{}", "/v1/admin/sources", "/v1/admin/policy"} {
		if !slices.Contains(paths, want) {
			t.Errorf("the console BFF paths %v do not carry %s", paths, want)
		}
	}
	for _, p := range paths {
		if !routes[p] {
			t.Errorf("the console BFF lets %s through, which api does not serve", p)
		}
		if strings.HasPrefix(p, "/v1/intents") || strings.HasPrefix(p, "/v1/accounts/operators") {
			t.Errorf("the console BFF lets the portal's %s through", p)
		}
	}
}
