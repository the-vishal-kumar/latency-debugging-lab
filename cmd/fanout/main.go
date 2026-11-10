// Command fanout simulates a request that fans out to N leaf servers (as a
// search query fans out to index shards) and prints how the tail grows with
// N, and what hedged requests and partial results buy back.
//
//	go run ./cmd/fanout
package main

import (
	"fmt"

	"github.com/the-vishal-kumar/latency-debugging-lab/internal/fanout"
)

func main() {
	m := fanout.Default
	const reqs = 20000
	fmt.Printf("Simulated, not measured. Leaf model: ~%.0f ms usually (lognormal); %.0f%% of calls take ~%.0f ms. %d requests per row, fixed seed.\n\n",
		m.FastMs, m.SlowProb*100, m.SlowMs, reqs)
	fmt.Println("### Waiting for every leaf\n\n| Leaves | p50 | p99 | Requests slower than 10 ms | Expected: 1 − 0.99^N |\n|---:|---:|---:|---:|---:|")
	for _, n := range []int{1, 10, 100, 1000} {
		r := fanout.Run(m, fanout.WaitAll, n, reqs, 0, 0, 1)
		fmt.Printf("| %d | %.1f ms | %.1f ms | %.1f%% | %.1f%% |\n", n, r.P50, r.P99, r.SlowFraction*100, m.Expected(n)*100)
	}
	const n = 100
	fmt.Printf("\n### %d leaves: three ways to wait\n\n| Strategy | p50 | p99 | Requests slower than 10 ms | Extra leaf requests | Leaves missing from the answer |\n|---|---:|---:|---:|---:|---:|\n", n)
	rows := []struct {
		name string
		s    fanout.Strategy
	}{{"wait for all", fanout.WaitAll}, {"hedge: resend to another replica after 2 ms", fanout.Hedge}, {"answer after 5 ms with what has arrived", fanout.PartialAfter}}
	for _, row := range rows {
		r := fanout.Run(m, row.s, n, reqs, 2, 5, 1)
		fmt.Printf("| %s | %.1f ms | %.1f ms | %.1f%% | %.1f%% | %.2f%% |\n", row.name, r.P50, r.P99, r.SlowFraction*100, r.ExtraRequests*100, r.MissingLeavesFraction*100)
	}
}
