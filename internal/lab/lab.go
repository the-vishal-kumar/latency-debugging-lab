// Package lab is a small multi-tenant shop API with five regressions that
// make pages slow, each switchable between "slow" and "fixed". The demo
// drives load at each variant and summarises what the traces show, so the
// signature of each cause can be read off a table.
package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/the-vishal-kumar/latency-debugging-lab/internal/obs"
)

// Tenants: 1 is small, 2 is big. Each has 1,000 customers.
const small, big, customersPerTenant = 1, 2, 1000

// Seed creates the shop schema: customers, and orders with an index on
// tenant_id only (the "existing" index that scenario 1 is about).
func Seed(ctx context.Context, db *pgxpool.Pool, smallOrders, bigOrders int) error {
	_, err := db.Exec(ctx, fmt.Sprintf(`
		DROP SCHEMA IF EXISTS shop CASCADE;
		CREATE SCHEMA shop;
		CREATE TABLE shop.customers (id int PRIMARY KEY, tenant_id int NOT NULL, name text NOT NULL,
		                             last_seen timestamptz, score int NOT NULL DEFAULT 0);
		INSERT INTO shop.customers (id, tenant_id, name)
		  SELECT (t - 1) * %[3]d + g, t, 'Customer ' || g FROM generate_series(1, %[3]d) g, generate_series(1, 2) t;
		CREATE TABLE shop.orders (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, tenant_id int NOT NULL,
		                          customer_id int NOT NULL, total numeric(10,2) NOT NULL, created_at timestamptz NOT NULL);
		INSERT INTO shop.orders (tenant_id, customer_id, total, created_at)
		  SELECT 1, 1 + floor(random() * %[3]d)::int, round((random() * 500)::numeric, 2), now() - random() * interval '365 days'
		  FROM generate_series(1, %[1]d);
		INSERT INTO shop.orders (tenant_id, customer_id, total, created_at)
		  SELECT 2, %[3]d + 1 + floor(random() * %[3]d)::int, round((random() * 500)::numeric, 2), now() - random() * interval '365 days'
		  FROM generate_series(1, %[2]d);
		CREATE INDEX orders_tenant ON shop.orders (tenant_id);
		ANALYZE shop.customers, shop.orders;`, smallOrders, bigOrders, customersPerTenant))
	return err
}

const compositeIndex = `CREATE INDEX IF NOT EXISTS orders_tenant_created ON shop.orders (tenant_id, created_at DESC)`

// ---- the app ---------------------------------------------------------------

type App struct {
	DB         *pgxpool.Pool
	Reports    *pgxpool.Pool // the pool reports run on; the same pool as DB unless fixed
	NPlusOne   bool          // load each order's customer with its own query
	DepURL     string        // recommendations service; empty = not called
	DepTimeout time.Duration // 0 = no timeout
}

type order struct {
	ID       int64     `json:"id"`
	Customer string    `json:"customer"`
	Total    float64   `json:"total"`
	Created  time.Time `json:"created_at"`
}

func (a *App) recentOrders(tenant int) obs.Handler {
	return func(ctx context.Context, _ *http.Request) ([]byte, error) {
		var out []order
		if a.NPlusOne {
			rows, err := a.DB.Query(ctx, `SELECT id, customer_id, total::float8, created_at FROM shop.orders
				WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 50`, tenant)
			if err != nil {
				return nil, err
			}
			var ids []int
			for rows.Next() {
				var o order
				var cid int
				if err := rows.Scan(&o.ID, &cid, &o.Total, &o.Created); err != nil {
					return nil, err
				}
				out, ids = append(out, o), append(ids, cid)
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
			for i, cid := range ids { // the N in N+1: one round trip per order
				if err := a.DB.QueryRow(ctx, `SELECT name FROM shop.customers WHERE id = $1`, cid).Scan(&out[i].Customer); err != nil {
					return nil, err
				}
			}
		} else {
			rows, err := a.DB.Query(ctx, `SELECT o.id, c.name, o.total::float8, o.created_at FROM shop.orders o
				JOIN shop.customers c ON c.id = o.customer_id
				WHERE o.tenant_id = $1 ORDER BY o.created_at DESC LIMIT 50`, tenant)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var o order
				if err := rows.Scan(&o.ID, &o.Customer, &o.Total, &o.Created); err != nil {
					return nil, err
				}
				out = append(out, o)
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}
		resp := map[string]any{"orders": out}
		if a.DepURL != "" {
			resp["recommended"] = a.recommendations(ctx)
		}
		return json.Marshal(resp)
	}
}

// recommendations calls another service. On error or timeout the page shows
// no recommendations rather than failing.
func (a *App) recommendations(ctx context.Context) []string {
	var recs []string
	_ = obs.Call(ctx, "recommendations", func(ctx context.Context) error {
		if a.DepTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, a.DepTimeout)
			defer cancel()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.DepURL, nil)
		if err != nil {
			return err
		}
		obs.Inject(ctx, req)
		resp, err := http.DefaultClient.Do(req) // http.DefaultClient has no timeout of its own
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("recommendations: status %d", resp.StatusCode)
		}
		return json.NewDecoder(resp.Body).Decode(&recs)
	})
	return recs
}

func (a *App) report(ctx context.Context, _ *http.Request) ([]byte, error) {
	// Stands in for a heavy report query: holds a connection for 300 ms.
	_, err := a.Reports.Exec(ctx, `SELECT pg_sleep(0.3)`)
	return []byte(`{}`), err
}

func (a *App) touch(ctx context.Context, r *http.Request) ([]byte, error) {
	id, err := strconv.Atoi(r.URL.Query().Get("customer"))
	if err != nil || id < 1 {
		return nil, obs.BadRequest("customer must be a positive integer")
	}
	_, err = a.DB.Exec(ctx, `UPDATE shop.customers SET last_seen = now() WHERE id = $1`, id)
	return []byte(`{}`), err
}

func (a *App) Mux() *http.ServeMux {
	m := http.NewServeMux()
	m.Handle("/orders/small", obs.Serve("/orders/small", a.recentOrders(small)))
	m.Handle("/orders/big", obs.Serve("/orders/big", a.recentOrders(big)))
	m.Handle("/report", obs.Serve("/report", a.report))
	m.Handle("/touch", obs.Serve("/touch", a.touch))
	return m
}

// ---- load and summaries ----------------------------------------------------

// Load is a group of clients hitting one path back to back.
type Load struct {
	Clients int
	Path    func() string
}

func drive(ctx context.Context, base string, d time.Duration, loads ...Load) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var wg sync.WaitGroup
	for _, l := range loads {
		for range l.Clients {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ctx.Err() == nil {
					req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+l.Path(), nil)
					resp, err := http.DefaultClient.Do(req) // finish the request in flight; don't count cancelled ones
					if err == nil {
						resp.Body.Close()
					}
				}
			}()
		}
	}
	wg.Wait()
	time.Sleep(100 * time.Millisecond) // let the last server spans end
}

// Stat is what the traces of one route say, averaged per request.
type Stat struct {
	Route             string
	Requests          int
	P50, P99          time.Duration
	Queries           float64       // db.query spans per request
	DB, Wait, Dep     time.Duration // per request: time in queries, waiting for a connection, in outgoing calls
	SlowestDBQueryP99 time.Duration // p99 of single-query durations
}

// spanRoute is the server span's http.route attribute.
func spanRoute(s sdktrace.ReadOnlySpan) string {
	for _, kv := range s.Attributes() {
		if kv.Key == "http.route" {
			return kv.Value.AsString()
		}
	}
	return s.Name()
}

func summarise(spans []sdktrace.ReadOnlySpan) []Stat {
	type acc struct {
		root          time.Duration
		route         string
		queries       int
		db, wait, dep time.Duration
		queryDurs     []time.Duration
	}
	byTrace := map[trace.TraceID]*acc{}
	get := func(id trace.TraceID) *acc {
		if byTrace[id] == nil {
			byTrace[id] = &acc{}
		}
		return byTrace[id]
	}
	for _, s := range spans {
		a, d := get(s.SpanContext().TraceID()), s.EndTime().Sub(s.StartTime())
		switch name := s.Name(); {
		case s.SpanKind() == trace.SpanKindServer:
			a.root, a.route = d, spanRoute(s)
		case name == "db.query":
			a.queries++
			a.db += d
			a.queryDurs = append(a.queryDurs, d)
		case name == "db.acquire":
			a.wait += d
		case strings.HasPrefix(name, "http.client"):
			a.dep += d
		}
	}
	routes := map[string][]*acc{}
	for _, a := range byTrace {
		if a.route != "" {
			routes[a.route] = append(routes[a.route], a)
		}
	}
	var out []Stat
	for route, as := range routes {
		st := Stat{Route: route, Requests: len(as)}
		var lat, q []time.Duration
		for _, a := range as {
			lat = append(lat, a.root)
			q = append(q, a.queryDurs...)
			st.Queries += float64(a.queries)
			st.DB += a.db
			st.Wait += a.wait
			st.Dep += a.dep
		}
		n := time.Duration(len(as))
		st.Queries /= float64(len(as))
		st.DB, st.Wait, st.Dep = st.DB/n, st.Wait/n, st.Dep/n
		slices.Sort(lat)
		st.P50, st.P99 = lat[len(lat)/2], lat[len(lat)*99/100]
		if len(q) > 0 {
			slices.Sort(q)
			st.SlowestDBQueryP99 = q[len(q)*99/100]
		}
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b Stat) int { return strings.Compare(a.Route, b.Route) })
	return out
}

// ---- scenarios ---------------------------------------------------------------

// Row is one variant of one scenario.
type Row struct {
	Variant string
	Stats   []Stat
	Note    string
}

type Scenario struct {
	Name, Symptom string
	Rows          []Row
}

type Env struct {
	URL      string // database URL
	Col      *obs.Collector
	Duration time.Duration // load per variant
}

func (e Env) run(ctx context.Context, a *App, loads ...Load) []Stat {
	srv := httptest.NewServer(a.Mux())
	defer srv.Close()
	e.Col.Take()
	drive(ctx, srv.URL, e.Duration, loads...)
	return summarise(e.Col.Take())
}

func constant(p string) func() string { return func() string { return p } }

// Index: the page is fast for small tenants and slow for big ones.
func (e Env) Index(ctx context.Context) (Scenario, error) {
	sc := Scenario{Name: "A query that grows with the tenant", Symptom: "slow only for big customers"}
	db, err := obs.Pool(ctx, e.URL, 20)
	if err != nil {
		return sc, err
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `DROP INDEX IF EXISTS shop.orders_tenant_created`); err != nil {
		return sc, err
	}
	a := &App{DB: db, Reports: db}
	loads := []Load{{4, constant("/orders/small")}, {4, constant("/orders/big")}}
	sc.Rows = append(sc.Rows, Row{"index on (tenant_id) only", e.run(ctx, a, loads...), ""})
	if _, err := db.Exec(ctx, compositeIndex); err != nil {
		return sc, err
	}
	sc.Rows = append(sc.Rows, Row{"index on (tenant_id, created_at DESC)", e.run(ctx, a, loads...), ""})
	return sc, nil
}

// NPlusOne: one query per order.
func (e Env) NPlusOne(ctx context.Context) (Scenario, error) {
	sc := Scenario{Name: "N+1 queries", Symptom: "slow everywhere, worse the further the database is"}
	db, err := obs.Pool(ctx, e.URL, 20)
	if err != nil {
		return sc, err
	}
	defer db.Close()
	if _, err := db.Exec(ctx, compositeIndex); err != nil {
		return sc, err
	}
	for _, n1 := range []bool{true, false} {
		a := &App{DB: db, Reports: db, NPlusOne: n1}
		v := "orders, then one query per order for the customer"
		if !n1 {
			v = "one query with a JOIN"
		}
		sc.Rows = append(sc.Rows, Row{v, e.run(ctx, a, Load{8, constant("/orders/small")}), ""})
	}
	return sc, nil
}

// Pool: reports hold the connections the page needs.
func (e Env) Pool(ctx context.Context) (Scenario, error) {
	sc := Scenario{Name: "Connection pool starvation", Symptom: "slow at peak or while reports run; queries themselves fast"}
	db, err := obs.Pool(ctx, e.URL, 5)
	if err != nil {
		return sc, err
	}
	defer db.Close()
	reports, err := obs.Pool(ctx, e.URL, 2)
	if err != nil {
		return sc, err
	}
	defer reports.Close()
	if _, err := db.Exec(ctx, compositeIndex); err != nil {
		return sc, err
	}
	loads := []Load{{16, constant("/orders/small")}, {6, constant("/report")}}
	sc.Rows = append(sc.Rows, Row{"one pool of 5 for pages and reports", e.run(ctx, &App{DB: db, Reports: db}, loads...), ""})
	sc.Rows = append(sc.Rows, Row{"pages: pool of 5; reports: their own pool of 2", e.run(ctx, &App{DB: db, Reports: reports}, loads...), ""})
	return sc, nil
}

// Dependency: another service is sometimes very slow, and there's no timeout.
func (e Env) Dependency(ctx context.Context) (Scenario, error) {
	sc := Scenario{Name: "A slow dependency with no timeout", Symptom: "slow sometimes, for some requests, with nothing wrong in our database"}
	dep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := 20 * time.Millisecond
		if rand.Float64() < 0.05 {
			d = 2 * time.Second // 5% of calls hit a cold cache, a GC pause, a retry storm...
		}
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`["a","b","c"]`))
	}))
	defer dep.Close()
	db, err := obs.Pool(ctx, e.URL, 20)
	if err != nil {
		return sc, err
	}
	defer db.Close()
	if _, err := db.Exec(ctx, compositeIndex); err != nil {
		return sc, err
	}
	for _, to := range []time.Duration{0, 100 * time.Millisecond} {
		v := "no timeout"
		if to > 0 {
			v = "100 ms timeout, page renders without recommendations"
		}
		a := &App{DB: db, Reports: db, DepURL: dep.URL, DepTimeout: to}
		sc.Rows = append(sc.Rows, Row{v, e.run(ctx, a, Load{8, constant("/orders/small")}), ""})
	}
	return sc, nil
}

// Lock: a batch job holds row locks the page's UPDATE needs.
func (e Env) Lock(ctx context.Context) (Scenario, error) {
	sc := Scenario{Name: "Waiting on a lock held by a batch job", Symptom: "slow while the job runs; CPU idle"}
	db, err := obs.Pool(ctx, e.URL, 20)
	if err != nil {
		return sc, err
	}
	defer db.Close()
	touch := func() string { return fmt.Sprintf("/touch?customer=%d", 1+rand.IntN(customersPerTenant)) }
	for _, chunked := range []bool{false, true} {
		jobCtx, stop := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { // the batch job: recompute every customer's score
			defer wg.Done()
			for jobCtx.Err() == nil {
				if !chunked {
					// One transaction over all 1,000 rows, with 500 ms of work inside.
					tx, err := db.Begin(jobCtx)
					if err != nil {
						return
					}
					_, _ = tx.Exec(jobCtx, `UPDATE shop.customers SET score = score + 1 WHERE tenant_id = 1`)
					_, _ = tx.Exec(jobCtx, `SELECT pg_sleep(0.5)`)
					_ = tx.Commit(jobCtx)
					continue
				}
				// 100 rows per transaction, the work done outside it.
				for lo := 1; lo <= customersPerTenant && jobCtx.Err() == nil; lo += 100 {
					_, _ = db.Exec(jobCtx, `UPDATE shop.customers SET score = score + 1 WHERE id BETWEEN $1 AND $2`, lo, lo+99)
					time.Sleep(50 * time.Millisecond)
				}
			}
		}()
		var samples, lockSamples int
		go func() { // what an on-call engineer would look at: pg_stat_activity
			defer wg.Done()
			for jobCtx.Err() == nil {
				var n int
				if err := db.QueryRow(jobCtx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'`).Scan(&n); err == nil {
					samples++
					if n > 0 {
						lockSamples++
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
		stats := e.run(ctx, &App{DB: db, Reports: db}, Load{8, touch})
		stop()
		wg.Wait()
		v := "job updates all rows in one 500 ms transaction"
		if chunked {
			v = "job updates 100 rows per short transaction"
		}
		sc.Rows = append(sc.Rows, Row{v, stats, fmt.Sprintf("pg_stat_activity showed sessions waiting on a lock in %d of %d samples", lockSamples, samples)})
	}
	return sc, nil
}

// All runs every scenario in order.
func (e Env) All(ctx context.Context) ([]Scenario, error) {
	var out []Scenario
	for _, f := range []func(context.Context) (Scenario, error){e.Index, e.NPlusOne, e.Pool, e.Dependency, e.Lock} {
		sc, err := f(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sc.Name, err)
		}
		out = append(out, sc)
	}
	return out, nil
}
