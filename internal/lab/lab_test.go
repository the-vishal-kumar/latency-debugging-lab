package lab

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/the-vishal-kumar/latency-debugging-lab/internal/obs"
)

func stat(t *testing.T, r Row, route string) Stat {
	t.Helper()
	for _, s := range r.Stats {
		if s.Route == route {
			return s
		}
	}
	t.Fatalf("%s: no requests to %s", r.Variant, route)
	return Stat{}
}

// Pins the trace signature of every regression, and that each fix removes it.
func TestSignatures(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `make test`")
	}
	ctx := context.Background()
	col, stop, err := obs.Setup(ctx, "shop-api-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	db, err := obs.Pool(ctx, url, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := Seed(ctx, db, 2000, 300_000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DROP SCHEMA IF EXISTS shop CASCADE`)
		db.Close()
	})
	e := Env{URL: url, Col: col, Duration: 1500 * time.Millisecond}

	t.Run("index", func(t *testing.T) {
		sc, err := e.Index(ctx)
		if err != nil {
			t.Fatal(err)
		}
		slow, fixed := stat(t, sc.Rows[0], "/orders/big"), stat(t, sc.Rows[1], "/orders/big")
		if slow.P50 < 10*fixed.P50 || slow.Queries != 1 {
			t.Errorf("big tenant should be one slow query before the index: %+v vs %+v", slow, fixed)
		}
		if small := stat(t, sc.Rows[1], "/orders/small"); fixed.P50 > 3*small.P50+time.Millisecond {
			t.Errorf("with the index, big and small tenants should be alike: %+v vs %+v", fixed, small)
		}
	})

	t.Run("n+1", func(t *testing.T) {
		sc, err := e.NPlusOne(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n, j := stat(t, sc.Rows[0], "/orders/small"), stat(t, sc.Rows[1], "/orders/small"); n.Queries != 51 || j.Queries != 1 || n.P50 < 3*j.P50 {
			t.Errorf("N+1 should be 51 queries and slower: %+v vs %+v", n, j)
		}
	})

	t.Run("pool", func(t *testing.T) {
		sc, err := e.Pool(ctx)
		if err != nil {
			t.Fatal(err)
		}
		shared, split := stat(t, sc.Rows[0], "/orders/small"), stat(t, sc.Rows[1], "/orders/small")
		if shared.Wait < 10*shared.DB {
			t.Errorf("a starved pool should show waiting for a connection, not slow queries: %+v", shared)
		}
		if split.P50 > shared.P50/5 {
			t.Errorf("a separate pool for reports should free the page: %+v vs %+v", split, shared)
		}
	})

	t.Run("dependency", func(t *testing.T) {
		sc, err := e.Dependency(ctx)
		if err != nil {
			t.Fatal(err)
		}
		none, to := stat(t, sc.Rows[0], "/orders/small"), stat(t, sc.Rows[1], "/orders/small")
		if none.P99 < time.Second || to.P99 > 500*time.Millisecond {
			t.Errorf("the timeout should cut the tail: %+v vs %+v", none, to)
		}
	})

	t.Run("lock", func(t *testing.T) {
		sc, err := e.Lock(ctx)
		if err != nil {
			t.Fatal(err)
		}
		slow, fixed := stat(t, sc.Rows[0], "/touch"), stat(t, sc.Rows[1], "/touch")
		if slow.P99 < 200*time.Millisecond || fixed.P99 > slow.P99/3 {
			t.Errorf("a long transaction should block the page's UPDATE: %+v vs %+v", slow, fixed)
		}
	})
}
