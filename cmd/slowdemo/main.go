// Command slowdemo makes a small API slow in five different ways, drives
// load at each one before and after the fix, and prints what the traces
// show for each, as Markdown.
//
//	go run ./cmd/slowdemo
//
// With OTEL_EXPORTER_OTLP_ENDPOINT set (docker compose sets it), every span
// also goes to Jaeger: open http://localhost:16686 to look at the traces.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/the-vishal-kumar/latency-debugging-lab/internal/lab"
	"github.com/the-vishal-kumar/latency-debugging-lab/internal/obs"
)

func main() {
	smallN := flag.Int("small", 2000, "orders for the small tenant")
	bigN := flag.Int("big", 1_000_000, "orders for the big tenant")
	dur := flag.Duration("duration", 3*time.Second, "load per variant")
	flag.Parse()
	if err := run(*smallN, *bigN, *dur); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(smallN, bigN int, dur time.Duration) error {
	ctx := context.Background()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://lab:lab@localhost:55433/lab?sslmode=disable"
	}
	col, stop, err := obs.Setup(ctx, "shop-api")
	if err != nil {
		return err
	}
	defer stop()
	db, err := obs.Pool(ctx, url, 4)
	if err != nil {
		return err
	}
	var v string
	if err := db.QueryRow(ctx, `SHOW server_version`).Scan(&v); err != nil {
		return err
	}
	if err := lab.Seed(ctx, db, smallN, bigN); err != nil {
		return err
	}
	db.Close()
	fmt.Printf("PostgreSQL %s. Small tenant: %d orders; big tenant: %d orders; 1,000 customers each. %s of load per variant.\n\n", v, smallN, bigN, dur)

	scs, err := lab.Env{URL: url, Col: col, Duration: dur}.All(ctx)
	if err != nil {
		return err
	}
	for i, sc := range scs {
		fmt.Printf("### %d. %s (%s)\n\n", i+1, sc.Name, sc.Symptom)
		fmt.Println("| Variant | Route | Requests | p50 | p99 | Queries/req | DB time/req | Waiting for a connection/req | Dependency/req |")
		fmt.Println("|---|---|---:|---:|---:|---:|---:|---:|---:|")
		for _, r := range sc.Rows {
			for _, s := range r.Stats {
				fmt.Printf("| %s | `%s` | %d | %s | %s | %.0f | %s | %s | %s |\n", r.Variant, s.Route, s.Requests,
					ms(s.P50), ms(s.P99), s.Queries, ms(s.DB), ms(s.Wait), ms(s.Dep))
			}
		}
		for _, r := range sc.Rows {
			if r.Note != "" {
				fmt.Printf("\n- %s: %s", r.Variant, r.Note)
			}
		}
		fmt.Print("\n\n")
	}
	return nil
}

func ms(d time.Duration) string {
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000)
}
