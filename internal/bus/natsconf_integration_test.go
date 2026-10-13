//go:build integration

package bus

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/rootxkit/uspace-ussp/internal/config"
)

// confURL is a NATS server started with deploy/compose/nats.conf (the
// Makefile's integration target: the compose stack's; CI starts one),
// whose users' passwords are USSP_NATS_<USER>_PASSWORD.
func confURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("USSP_TEST_NATS_CONF_URL")
	if u == "" {
		t.Fatal("USSP_TEST_NATS_CONF_URL is not set: a NATS started with deploy/compose/nats.conf; see the Makefile's integration target")
	}
	return u
}

// confUser is a connection as process p's own NATS user, with the
// permission violations the server reports on errs.
func confUser(t *testing.T, p string) (*nats.Conn, <-chan error) {
	t.Helper()
	env := "USSP_NATS_" + strings.ToUpper(strings.ReplaceAll(p, "-", "_")) + "_PASSWORD"
	pw := os.Getenv(env)
	if pw == "" {
		t.Fatalf("%s is not set", env)
	}
	errs := make(chan error, 64)
	nc, err := nats.Connect(confURL(t), nats.UserInfo(p, pw), nats.Name("natsconf-test-"+p), nats.MaxReconnects(0),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			select {
			case errs <- err:
			default:
			}
		}))
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	t.Cleanup(nc.Close)
	return nc, errs
}

// refused publishes subject as nc and is the server's permission
// violation for it, waiting at most a few seconds.
func refused(t *testing.T, nc *nats.Conn, errs <-chan error, subject string) bool {
	t.Helper()
	if err := nc.Publish(subject, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := nc.FlushTimeout(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case err := <-errs:
			if errors.Is(err, nats.ErrPermissionViolation) && strings.Contains(err.Error(), subject) {
				return true
			}
		case <-deadline.C:
			return false
		}
	}
}

// The integration twin of internal/app/proc TestNATSConfJWKSCacheWriters,
// on a server started with deploy/compose/nats.conf. Presence: each
// process that verifies tokens creates jwks_cache and puts and deletes a
// key under its own prefix. Absence: it cannot write another process's
// prefix; a process that verifies nothing can neither write the bucket
// nor create it; nobody can update, delete or purge its stream or delete
// a message from it.
func TestNATSConfJWKSCacheWritersOnAServer(t *testing.T) {
	var writers []string
	for _, v := range config.Variables() {
		if v.Name == "USSP_JWKS_CACHE_KEY_FILE" {
			writers = strings.Split(v.ReadBy, ",")
		}
	}
	if len(writers) == 0 {
		t.Fatal("no process reads USSP_JWKS_CACHE_KEY_FILE")
	}
	bucket, _ := DefaultTopology().Bucket(BucketJWKSCache)
	for _, p := range config.Processes {
		t.Run(p, func(t *testing.T) {
			nc, errs := confUser(t, p)
			if slices.Contains(writers, p) {
				js, err := jetstream.New(nc)
				if err != nil {
					t.Fatal(err)
				}
				c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if _, err := Ensure(c, js, Topology{Buckets: []jetstream.KeyValueConfig{bucket}}); err != nil {
					t.Fatalf("%s cannot create jwks_cache: %v", p, err)
				}
				kv, err := js.KeyValue(c, BucketJWKSCache)
				if err != nil {
					t.Fatal(err)
				}
				own := p + ".natsconf-test"
				if _, err := kv.Put(c, own, []byte("{}")); err != nil {
					t.Fatalf("%s cannot write its own prefix: %v", p, err)
				}
				if err := kv.Delete(c, own); err != nil {
					t.Fatalf("%s cannot delete its own key: %v", p, err)
				}
				for _, o := range config.Processes {
					if o != p && !refused(t, nc, errs, "$KV.jwks_cache."+o+".natsconf-test") {
						t.Errorf("%s wrote %s's prefix", p, o)
					}
				}
			} else {
				if !refused(t, nc, errs, "$KV.jwks_cache."+p+".natsconf-test") {
					t.Errorf("%s, which verifies nothing, wrote jwks_cache", p)
				}
				if !refused(t, nc, errs, "$JS.API.STREAM.CREATE.KV_jwks_cache") {
					t.Errorf("%s, which verifies nothing, may create jwks_cache", p)
				}
			}
			for _, api := range []string{"UPDATE", "DELETE", "PURGE", "MSG.DELETE"} {
				if !refused(t, nc, errs, "$JS.API.STREAM."+api+".KV_jwks_cache") {
					t.Errorf("%s may %s jwks_cache's stream", p, api)
				}
			}
			if !refused(t, nc, errs, "$KV.jwks_cache.unprefixed") {
				t.Errorf("%s wrote an unprefixed key", p)
			}
		})
	}
}
