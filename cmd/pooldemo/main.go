// Command pooldemo shows "too many connections": several app instances,
// each with its own pool of 20, against PostgreSQL's default
// max_connections of 100, directly and through PgBouncer.
//
//	pooldemo -direct postgres://...:5432/lab -bouncer postgres://...:6432/lab
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	direct := flag.String("direct", "", "PostgreSQL URL")
	bouncer := flag.String("bouncer", "", "PgBouncer URL")
	instances := flag.String("instances", "4,6", "app instances to simulate")
	per := flag.Int("pool", 20, "pool size per instance")
	d := flag.Duration("duration", 8*time.Second, "load per run")
	flag.Parse()
	if *direct == "" || *bouncer == "" {
		fmt.Fprintln(os.Stderr, "pooldemo: -direct and -bouncer are both required (scaling/run.sh sets them)")
		os.Exit(2)
	}
	var counts []int
	for _, s := range strings.Split(*instances, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 {
			fmt.Fprintf(os.Stderr, "pooldemo: -instances: %q is not a positive integer\n", s)
			os.Exit(2)
		}
		counts = append(counts, n)
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, *direct)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer admin.Close()
	var maxConns string
	if err := admin.QueryRow(ctx, `SHOW max_connections`).Scan(&maxConns); err != nil {
		fmt.Fprintln(os.Stderr, "pooldemo: cannot reach PostgreSQL:", err)
		os.Exit(1)
	}
	fmt.Printf("PostgreSQL max_connections = %s; each instance has a pool of %d and runs %d concurrent queries that each need ~5 ms of database CPU.\n\n", maxConns, *per, *per)
	fmt.Println("| Path | Instances | Client connections wanted | Server connections used (peak) | Queries/s | p50 | p99 | Errors |")
	fmt.Println("|---|---:|---:|---:|---:|---:|---:|---|")
	for _, path := range []struct{ name, url string }{{"direct to PostgreSQL", *direct}, {"through PgBouncer (transaction mode, pool 20)", *bouncer}} {
		for _, n := range counts {
			if err := run(ctx, admin, path.name, path.url, n, *per, *d); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			time.Sleep(2 * time.Second)
		}
	}
}

func run(ctx context.Context, admin *pgxpool.Pool, name, url string, instances, per int, d time.Duration) error {
	var pools []*pgxpool.Pool
	for range instances {
		cfg, err := pgxpool.ParseConfig(url)
		if err != nil {
			return err
		}
		cfg.MaxConns = int32(per)
		p, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			return err
		}
		pools = append(pools, p)
	}
	defer func() {
		for _, p := range pools {
			p.Close()
		}
	}()
	var done, fails atomic.Int64
	var mu sync.Mutex
	var lat []time.Duration
	errKinds := map[string]int{}
	var peak atomic.Int64
	ctx2, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // what the database sees
		defer wg.Done()
		for ctx2.Err() == nil {
			var c int64
			if admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND usename = 'lab'`).Scan(&c) == nil {
				peak.Store(max(peak.Load(), c-1)) // minus this monitoring connection
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	for _, p := range pools {
		for range per {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var mine []time.Duration
				for ctx2.Err() == nil {
					t := time.Now()
					_, err := p.Exec(ctx2, `SELECT count(*) FROM generate_series(1, 30000)`) // ~5 ms of database CPU
					if ctx2.Err() != nil {
						break
					}
					if err != nil {
						fails.Add(1)
						kind := err.Error()
						var pe *pgconn.PgError
						if errors.As(err, &pe) {
							kind = pe.Code + " " + pe.Message
						}
						mu.Lock()
						errKinds[kind]++
						mu.Unlock()
						time.Sleep(50 * time.Millisecond) // an app would back off and retry
						continue
					}
					done.Add(1)
					mine = append(mine, time.Since(t))
				}
				mu.Lock()
				lat = append(lat, mine...)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	slices.Sort(lat)
	p := func(q float64) string {
		if len(lat) == 0 {
			return "—"
		}
		return fmt.Sprintf("%.1f ms", float64(lat[int(float64(len(lat))*q)].Microseconds())/1000)
	}
	errs := "0"
	if f := fails.Load(); f > 0 {
		var parts []string
		for k, v := range errKinds {
			parts = append(parts, fmt.Sprintf("%d × `%s`", v, k))
		}
		slices.Sort(parts)
		errs = strings.Join(parts, "; ")
	}
	fmt.Printf("| %s | %d | %d | %d | %.0f | %s | %s | %s |\n", name, instances, instances*per, peak.Load(),
		float64(done.Load())/d.Seconds(), p(0.5), p(0.99), errs)
	return nil
}
