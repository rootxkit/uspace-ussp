package proc

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rootxkit/uspace-ussp/internal/bus"
	"github.com/rootxkit/uspace-ussp/internal/config"
)

// natsConfs are the NATS configurations the deployments start with.
var natsConfs = []string{
	filepath.Join("..", "..", "..", "deploy", "compose", "nats.conf"),
	filepath.Join("..", "..", "..", "deploy", "compose", "nats.staging.conf"),
}

var (
	natsUserBlock = regexp.MustCompile(`(?s)\{\s*user:\s*([a-z-]+)\s*password:[^\n]*\n(.*?)\n      \}`)
	natsDeny      = regexp.MustCompile(`(?s)deny:\s*\[(.*?)\]`)
	natsQuoted    = regexp.MustCompile(`"([^"]+)"`)
)

// natsPublishDenies is each user's publish deny list in conf.
func natsPublishDenies(t *testing.T, conf string) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, m := range natsUserBlock.FindAllStringSubmatch(string(b), -1) {
		var deny []string
		if d := natsDeny.FindStringSubmatch(m[2]); d != nil {
			for _, q := range natsQuoted.FindAllStringSubmatch(d[1], -1) {
				deny = append(deny, q[1])
			}
		}
		out[m[1]] = deny
	}
	return out
}

// jwks_cache is written only by the processes that verify tokens
// (JWKSCacheWriters, from the variable they read), each under its own
// user's prefix: every NATS configuration denies each user the other
// processes' prefixes and never its own, denies the processes that
// verify nothing the bucket and its creation, and denies everyone the
// stream's update, deletion, purge and message deletion. The topology a
// process maintains holds jwks_cache exactly when it may create it.
// The integration twin, internal/bus TestNATSConfJWKSCacheWritersOnAServer,
// runs the same rules on a server.
func TestNATSConfJWKSCacheWriters(t *testing.T) {
	writers := JWKSCacheWriters()
	if len(writers) == 0 || slices.Contains(writers, config.ProcessDSSSync) || !slices.Contains(writers, config.ProcessAPI) {
		t.Fatalf("writers %v", writers)
	}
	always := []string{"$KV.jwks_cache.*", "$JS.API.STREAM.UPDATE.KV_jwks_cache", "$JS.API.STREAM.DELETE.KV_jwks_cache",
		"$JS.API.STREAM.PURGE.KV_jwks_cache", "$JS.API.STREAM.MSG.DELETE.KV_jwks_cache"}
	for _, conf := range natsConfs {
		denies := natsPublishDenies(t, conf)
		for _, p := range config.Processes {
			deny, ok := denies[p]
			if !ok {
				t.Errorf("%s: no user %s with a deny list", conf, p)
				continue
			}
			for _, d := range always {
				if !slices.Contains(deny, d) {
					t.Errorf("%s: %s may publish %s", conf, p, d)
				}
			}
			create := "$JS.API.STREAM.CREATE.KV_jwks_cache"
			if !slices.Contains(writers, p) {
				if !slices.Contains(deny, "$KV.jwks_cache.>") || !slices.Contains(deny, create) {
					t.Errorf("%s: %s verifies no token and may write or create jwks_cache: %v", conf, p, deny)
				}
				continue
			}
			if slices.Contains(deny, "$KV.jwks_cache.>") || slices.Contains(deny, create) {
				t.Errorf("%s: the writer %s may not write or create jwks_cache", conf, p)
			}
			for _, o := range config.Processes {
				denied := slices.Contains(deny, "$KV.jwks_cache."+o+".>")
				if o == p && denied {
					t.Errorf("%s: %s is denied its own prefix", conf, p)
				}
				if o != p && !denied {
					t.Errorf("%s: %s may write %s's prefix", conf, p, o)
				}
			}
		}
	}
	for _, p := range config.Processes {
		_, has := TopologyFor(p, config.Config{}).Bucket(bus.BucketJWKSCache)
		if has != slices.Contains(writers, p) {
			t.Errorf("%s maintains jwks_cache: %v", p, has)
		}
	}
	if len(TopologyFor(config.ProcessDSSSync, config.Config{}).Buckets) != len(TopologyOf(config.Config{}).Buckets)-1 {
		t.Error("TopologyFor dropped more than jwks_cache")
	}
}

var natsAllow = regexp.MustCompile(`(?s)allow:\s*\[(.*?)\]`)

// The staging backup user reads the KV buckets and nothing else: its
// publish allow list is the account's stream listing and, for each
// bucket of the topology and no other stream, that bucket's stream
// info, direct and message gets, the ordered consumer a key listing
// creates and its flow control. Consumer deletion is denied (a
// process's durable consumer included). The smoke run exercises the
// list on a server (deploy/smoke/run.sh, step 6).
func TestNATSStagingBackupUser(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "compose", "nats.staging.conf"))
	if err != nil {
		t.Fatal(err)
	}
	var block string
	for _, m := range natsUserBlock.FindAllStringSubmatch(string(b), -1) {
		if m[1] == "backup" {
			block = m[2]
		}
	}
	if block == "" {
		t.Fatal("no backup user in nats.staging.conf")
	}
	var allow []string
	if a := natsAllow.FindStringSubmatch(block); a != nil {
		for _, q := range natsQuoted.FindAllStringSubmatch(a[1], -1) {
			allow = append(allow, q[1])
		}
	}
	want := []string{"$JS.API.INFO", "$JS.API.STREAM.NAMES", "$JS.API.STREAM.LIST"}
	for _, bk := range TopologyOf(config.Config{}).Buckets {
		k := "KV_" + bk.Bucket
		want = append(want, "$JS.API.STREAM.INFO."+k, "$JS.API.STREAM.MSG.GET."+k, "$JS.API.DIRECT.GET."+k, "$JS.API.DIRECT.GET."+k+".>",
			"$JS.API.CONSUMER.CREATE."+k, "$JS.API.CONSUMER.CREATE."+k+".>", "$JS.API.CONSUMER.INFO."+k+".>", "$JS.FC."+k+".>")
	}
	slices.Sort(allow)
	slices.Sort(want)
	if !slices.Equal(allow, want) {
		for _, a := range allow {
			if !slices.Contains(want, a) {
				t.Errorf("the backup user may publish %s", a)
			}
		}
		for _, w := range want {
			if !slices.Contains(allow, w) {
				t.Errorf("the backup user may not publish %s: that bucket is not backed up", w)
			}
		}
	}
	d := natsDeny.FindStringSubmatch(block)
	if len(d) < 2 || !strings.Contains(d[1], `"$JS.API.CONSUMER.DELETE.>"`) {
		t.Errorf("the backup user may delete a consumer: %v", d)
	}
}
