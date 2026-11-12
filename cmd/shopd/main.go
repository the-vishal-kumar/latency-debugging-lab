// Command shopd is the API the scaling experiments run against: one process,
// configured by environment variables, run as one or several replicas.
//
//	GET /cpu     about 1 ms of CPU work per request (rendering, JSON, auth)
//	GET /stats   a per-tenant aggregate over 100,000 orders (a costly read)
//
// CACHE=none|local|redis caches /stats for 1 s. RATE_LIMIT=n allows each
// client (the X-Client header) n requests/s, with a burst of n.
//
// shopd -healthcheck exits 0 if a shopd on :8080 answers /healthz; the
// image has no shell, so this is its Docker healthcheck.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	db, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	iter, err := strconv.Atoi(env("CPU_ITER", "2000"))
	if err != nil || iter < 0 {
		log.Fatalf("CPU_ITER must be a non-negative integer, got %q", os.Getenv("CPU_ITER"))
	}
	s := &server{db: db, iter: iter, cache: env("CACHE", "none"), local: map[string]entry{}}
	if s.cache == "redis" {
		opt, err := redis.ParseURL(os.Getenv("REDIS_URL"))
		if err != nil {
			log.Fatal(err)
		}
		s.rdb = redis.NewClient(opt)
	}
	if s.cache != "none" && s.cache != "local" && s.cache != "redis" {
		log.Fatalf("CACHE must be none, local or redis, got %q", s.cache)
	}
	var h http.Handler = s.mux()
	n, err := strconv.Atoi(env("RATE_LIMIT", "0"))
	if err != nil || n < 0 {
		log.Fatalf("RATE_LIMIT must be a non-negative integer, got %q", os.Getenv("RATE_LIMIT"))
	}
	if n > 0 {
		h = limit(h, float64(n))
	}
	srv := &http.Server{
		Addr:              ":8080",
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut) // finish requests in flight, then return
	}()
	log.Printf("shopd: cache=%s rate_limit=%d cpu_iter=%d", s.cache, n, iter)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	log.Print("shopd: stopped")
}

func healthcheck() int {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://127.0.0.1:8080/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type entry struct {
	body    []byte
	expires time.Time
}

type server struct {
	db    *pgxpool.Pool
	rdb   *redis.Client
	iter  int
	cache string
	mu    sync.Mutex
	local map[string]entry
}

func (s *server) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/cpu", func(w http.ResponseWriter, r *http.Request) {
		sum := sha256.Sum256([]byte(r.URL.RawQuery))
		for range s.iter { // CPU work that scales with cores, not with waiting
			sum = sha256.Sum256(sum[:])
		}
		fmt.Fprintf(w, `{"digest":"%x"}`, sum[:4])
	})
	m.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		tenant, err := strconv.Atoi(r.URL.Query().Get("tenant"))
		if err != nil || tenant < 1 {
			http.Error(w, "tenant must be a positive integer", http.StatusBadRequest)
			return
		}
		body, err := s.stats(r.Context(), tenant)
		if err != nil {
			log.Printf("stats: %v", err) // the detail goes to the log, not the client
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=1") // lets a CDN or nginx cache it for 1 s
		_, _ = w.Write(body)
	})
	m.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	return m
}

func (s *server) stats(ctx context.Context, tenant int) ([]byte, error) {
	key := "stats:" + strconv.Itoa(tenant)
	switch s.cache {
	case "local":
		s.mu.Lock()
		e, ok := s.local[key]
		s.mu.Unlock()
		if ok && time.Now().Before(e.expires) {
			return e.body, nil
		}
	case "redis":
		if b, err := s.rdb.Get(ctx, key).Bytes(); err == nil {
			return b, nil
		}
	}
	var n int64
	var total float64
	if err := s.db.QueryRow(ctx, `SELECT count(*), coalesce(sum(total), 0)::float8 FROM scale.orders WHERE tenant_id = $1`,
		tenant).Scan(&n, &total); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"tenant": tenant, "orders": n, "revenue": total})
	switch s.cache {
	case "local":
		s.mu.Lock()
		s.local[key] = entry{body, time.Now().Add(time.Second)}
		s.mu.Unlock()
	case "redis":
		_ = s.rdb.Set(ctx, key, body, time.Second).Err()
	}
	return body, nil
}

// limit is a token bucket per client: rate tokens/s, burst = rate.
//
// The client is whatever X-Client says, so it is only as trustworthy as
// whoever sets that header (here, the load generator via nginx). Buckets
// idle for a minute are dropped, so a stream of new client IDs can't grow
// the map without bound.
func limit(next http.Handler, rate float64) http.Handler {
	type bucket struct {
		tokens float64
		last   time.Time
	}
	var mu sync.Mutex
	buckets := map[string]*bucket{}
	lastSweep := time.Now()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Client")
		now := time.Now()
		mu.Lock()
		if now.Sub(lastSweep) > time.Minute {
			for k, b := range buckets {
				if now.Sub(b.last) > time.Minute {
					delete(buckets, k)
				}
			}
			lastSweep = now
		}
		b := buckets[id]
		if b == nil {
			b = &bucket{tokens: rate, last: now}
			buckets[id] = b
		}
		b.tokens = min(rate, b.tokens+now.Sub(b.last).Seconds()*rate)
		b.last = now
		ok := b.tokens >= 1
		if ok {
			b.tokens--
		}
		mu.Unlock()
		if !ok {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
