package fanout

import (
	"math"
	"testing"
)

func TestTail(t *testing.T) {
	m := Default
	for _, n := range []int{1, 10, 100} {
		r := Run(m, WaitAll, n, 20000, 0, 0, 7)
		if math.Abs(r.SlowFraction-m.Expected(n)) > 0.02 {
			t.Errorf("n=%d: slow fraction %.3f, expected %.3f", n, r.SlowFraction, m.Expected(n))
		}
	}
	all := Run(m, WaitAll, 100, 20000, 2, 5, 7)
	hedge := Run(m, Hedge, 100, 20000, 2, 5, 7)
	// A backup can hit a hiccup too, so hedging leaves about p² of the
	// leaves slow instead of p: most slow requests go, not all.
	if all.SlowFraction < 0.5 || hedge.SlowFraction > 0.03 || hedge.P50 > 5 || hedge.ExtraRequests > 0.05 {
		t.Errorf("hedging should remove most slow requests for a few %% extra leaf calls: all %+v, hedge %+v", all, hedge)
	}
	part := Run(m, PartialAfter, 100, 20000, 2, 5, 7)
	if part.P99 > 5.01 || part.MissingLeavesFraction > 0.02 {
		t.Errorf("partial results should cap latency and miss few leaves: %+v", part)
	}
}
