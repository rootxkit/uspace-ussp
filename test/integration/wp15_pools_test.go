//go:build integration

package integration

import (
	"sync"
	"testing"

	"github.com/rootxkit/uspace-ussp/internal/store"
)

// testPools holds one pool per test and role for the WP-15 tests. Their
// helpers run inside waitFor loops; a pool opened per call (relApp,
// tsOwner) would stay open until the test ends, and a few hundred polls
// exhaust the server's connection slots (SQLSTATE 53300, seen in CI).
var testPools sync.Map

type testPoolKey struct {
	t    *testing.T
	kind string
}

func cachedPool(t *testing.T, kind string, open func(*testing.T) *store.Pool) *store.Pool {
	t.Helper()
	k := testPoolKey{t: t, kind: kind}
	if p, ok := testPools.Load(k); ok {
		return p.(*store.Pool)
	}
	p := open(t) // closed by open's own cleanup
	t.Cleanup(func() { testPools.Delete(k) })
	testPools.Store(k, p)
	return p
}

// appPool is relApp, one pool for the whole test.
func appPool(t *testing.T) *store.Pool { return cachedPool(t, "rel-app", relApp) }

// tsPool is tsOwner, one pool for the whole test.
func tsPool(t *testing.T) *store.Pool { return cachedPool(t, "ts-owner", tsOwner) }
