// Package fanout shows why a request that fans out to many servers is slow
// at the tail even when each server is fast, and what hedged requests and
// partial results buy back. It's a simulation from a stated latency model
// (not a measurement), so the effect of fan-out alone is visible and every
// number is reproducible from the seed.
package fanout

import (
	"math"
	"math/rand/v2"
	"slices"
)

// Model: a leaf usually answers in about 1 ms (lognormal around 1 ms), but
// SlowProb of the time it hits a hiccup (a GC pause, a busy neighbour, a
// queue behind a big request) and takes SlowMs.
type Model struct {
	FastMs, SlowMs, SlowProb float64
}

var Default = Model{FastMs: 1, SlowMs: 50, SlowProb: 0.01}

func (m Model) leaf(r *rand.Rand) float64 {
	if r.Float64() < m.SlowProb {
		return m.SlowMs * (0.8 + 0.4*r.Float64())
	}
	return m.FastMs * math.Exp(0.25*r.NormFloat64())
}

// Strategy decides how long the root waits for N leaves, and how many extra
// requests it sends.
type Strategy int

const (
	WaitAll      Strategy = iota // wait for every leaf
	Hedge                        // after HedgeAfterMs, send a second copy of each unanswered leaf to another replica; use whichever answers first
	PartialAfter                 // answer after PartialMs with whatever has arrived
)

type Result struct {
	Leaves                int
	P50, P99              float64 // request latency, ms
	SlowFraction          float64 // requests slower than 10 ms
	ExtraRequests         float64 // hedges per request, as a fraction of leaves
	MissingLeavesFraction float64 // partial results: leaves not included, on average
}

func pct(v []float64, q float64) float64 {
	slices.Sort(v)
	return v[min(len(v)-1, int(float64(len(v))*q))]
}

// Run simulates requests requests to n leaves.
func Run(m Model, s Strategy, n, requests int, hedgeAfterMs, partialMs float64, seed uint64) Result {
	r := rand.New(rand.NewPCG(seed, 42))
	lat := make([]float64, requests)
	var slow, extra, missing float64
	for i := range requests {
		worst, absent := 0.0, 0
		for range n {
			t := m.leaf(r)
			switch s {
			case Hedge:
				if t > hedgeAfterMs {
					extra++
					t = math.Min(t, hedgeAfterMs+m.leaf(r)) // the backup, sent late, races the original
				}
			case PartialAfter:
				if t > partialMs {
					absent++
					t = partialMs
				}
			}
			worst = math.Max(worst, t)
		}
		lat[i] = worst
		if worst > 10 {
			slow++
		}
		missing += float64(absent) / float64(n)
	}
	q := float64(requests)
	return Result{Leaves: n, P50: pct(lat, 0.5), P99: pct(lat, 0.99), SlowFraction: slow / q,
		ExtraRequests: extra / q / float64(n), MissingLeavesFraction: missing / q}
}

// Expected fraction of requests that hit at least one slow leaf: 1 - (1-p)^n.
func (m Model) Expected(n int) float64 { return 1 - math.Pow(1-m.SlowProb, float64(n)) }
