// Command loadgen sends requests at a fixed arrival rate (an open model:
// new requests keep arriving whether or not earlier ones have finished, as
// with real users) and reports latency and errors.
//
// Find the highest rate a setup sustains:
//
//	loadgen -url http://nginx/cpu -rates 200,400,800 -label "1 replica"
//
// Normal clients plus one noisy client:
//
//	loadgen -url http://nginx/cpu -rates 300 -noisy-rate 1500 -label "no limit"
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type result struct {
	sent, ok, limited, errs int
	lat                     []time.Duration // of 2xx responses
}

func (r *result) p(q float64) time.Duration {
	if len(r.lat) == 0 {
		return 0
	}
	slices.Sort(r.lat)
	return r.lat[min(len(r.lat)-1, int(float64(len(r.lat))*q))]
}

var client = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{MaxIdleConns: 4000, MaxIdleConnsPerHost: 4000, IdleConnTimeout: 30 * time.Second},
}

// fire sends rate requests/s for d, each from a client ID chosen by id().
func fire(ctx context.Context, url string, rate float64, d time.Duration, id func() string) *result {
	r := &result{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	interval := time.Duration(float64(time.Second) / rate)
	start := time.Now()
	for i := 0; ; i++ {
		at := start.Add(time.Duration(i) * interval)
		if at.Sub(start) >= d {
			break
		}
		time.Sleep(time.Until(at))
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			req.Header.Set("X-Client", id())
			t := time.Now()
			resp, err := client.Do(req)
			lat := time.Since(t)
			mu.Lock()
			defer mu.Unlock()
			r.sent++
			if err != nil {
				r.errs++
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusTooManyRequests:
				r.limited++
			case resp.StatusCode >= 300:
				r.errs++
			default:
				r.ok++
				r.lat = append(r.lat, lat)
			}
		}()
	}
	wg.Wait()
	return r
}

func ms(d time.Duration) string {
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000)
}

func main() {
	url := flag.String("url", "http://nginx/cpu", "target")
	rates := flag.String("rates", "100", "comma-separated arrival rates (req/s), tried in order")
	d := flag.Duration("duration", 8*time.Second, "per rate")
	clients := flag.Int("clients", 10, "normal client IDs (X-Client)")
	noisy := flag.Float64("noisy-rate", 0, "extra req/s from one client, X-Client: noisy")
	label := flag.String("label", "", "row label")
	slo := flag.Duration("slo", 100*time.Millisecond, "p99 a rate must stay under to count as sustained")
	query := flag.String("query", "", "if set, appended as ?<query>=<random 1..10>")
	flag.Parse()
	var rateList []float64
	for _, s := range strings.Split(*rates, ",") {
		rate, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || rate <= 0 {
			fmt.Fprintf(os.Stderr, "loadgen: -rates: %q is not a positive number\n", s)
			os.Exit(2)
		}
		rateList = append(rateList, rate)
	}
	if *clients < 1 || *d <= 0 || *noisy < 0 {
		fmt.Fprintln(os.Stderr, "loadgen: -clients must be at least 1, -duration positive, -noisy-rate not negative")
		os.Exit(2)
	}
	ctx := context.Background()
	// One warm-up second at the first rate.
	first := rateList[0]
	fireURL := func(rate float64, dur time.Duration, id func() string) *result {
		if *query == "" {
			return fire(ctx, *url, rate, dur, id)
		}
		// Spread over 10 tenants: one fire per tenant at rate/10.
		var mu sync.Mutex
		all := &result{}
		var wg sync.WaitGroup
		for t := 1; t <= 10; t++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := fire(ctx, fmt.Sprintf("%s?%s=%d", *url, *query, t), rate/10, dur, id)
				mu.Lock()
				all.sent, all.ok, all.limited, all.errs = all.sent+r.sent, all.ok+r.ok, all.limited+r.limited, all.errs+r.errs
				all.lat = append(all.lat, r.lat...)
				mu.Unlock()
			}()
		}
		wg.Wait()
		return all
	}
	normalID := func() string { return "client-" + strconv.Itoa(rand.IntN(*clients)) }
	fireURL(first, time.Second, normalID)

	if *noisy > 0 {
		var wg sync.WaitGroup
		var nr *result
		wg.Add(1)
		go func() { defer wg.Done(); nr = fire(ctx, *url, *noisy, *d, func() string { return "noisy" }) }()
		r := fireURL(first, *d, normalID)
		wg.Wait()
		fmt.Printf("| %s | %.0f | %s | %s | %d | %.0f | %d | %d |\n", *label, float64(r.ok)/d.Seconds(), ms(r.p(0.5)), ms(r.p(0.99)),
			r.errs+r.limited, float64(nr.ok)/d.Seconds(), nr.limited, nr.errs)
		return
	}

	type step struct {
		rate float64
		r    *result
	}
	var steps []step
	best := -1
	for _, rate := range rateList {
		r := fireURL(rate, *d, normalID)
		steps = append(steps, step{rate, r})
		achieved := float64(r.ok) / d.Seconds()
		fmt.Fprintf(os.Stderr, "%s: rate %.0f -> ok %.0f/s, p50 %s, p99 %s, errors %d\n", *label, rate, achieved, ms(r.p(0.5)), ms(r.p(0.99)), r.errs)
		if r.p(0.99) <= *slo && r.errs*100 <= r.sent && achieved >= 0.95*rate {
			best = len(steps) - 1
		} else {
			break // past the knee: higher rates only queue more
		}
		time.Sleep(2 * time.Second) // let queues drain between steps
	}
	if best < 0 {
		fmt.Printf("| %s | below %.0f | - | - | - |\n", *label, steps[0].rate)
		return
	}
	b := steps[best]
	next := "-"
	if best+1 < len(steps) {
		n := steps[best+1]
		next = fmt.Sprintf("%.0f req/s: p99 %s, %d errors", n.rate, ms(n.r.p(0.99)), n.r.errs)
	}
	fmt.Printf("| %s | %.0f | %s | %s | %s |\n", *label, b.rate, ms(b.r.p(0.5)), ms(b.r.p(0.99)), next)
}
