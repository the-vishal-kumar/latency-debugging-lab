# latency-debugging-lab

[![CI](https://github.com/the-vishal-kumar/latency-debugging-lab/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/the-vishal-kumar/latency-debugging-lab/actions/workflows/ci.yml)

Find the bottleneck, then scale it. Two halves:

1. **Debugging** (`make demo`): a small multi-tenant API, made slow in five common ways. For each one, the demo drives load before and after the fix and summarises what the traces show, so you can learn each cause's **signature**: the shape it leaves in a trace before you know what it is.
2. **Scaling** (`make scaling`): what each scaling technique buys, measured. More replicas vs a bigger instance, four places to cache, a connection pooler vs a bigger database, and per-client rate limits against a noisy client. Results: [scaling/results.md](scaling/results.md).

```bash
make demo   # about a minute; prints results as Markdown
make test   # pins each signature
make down
```

Without `make`: `docker compose up -d --wait postgres jaeger && docker compose run --rm go go run ./cmd/slowdemo`. You need Docker; Go is optional. While or after it runs, open **http://localhost:16686** (Jaeger) and look at the `shop-api` traces: every request has a server span, one span per database query, one per wait for a pooled connection, and one per outgoing call. Every response also carries a `Server-Timing` header (`db`, `db-wait`, `dep`, `total`), which Chrome and Firefox DevTools show in the Network panel's Timing tab, so the same breakdown is visible from the browser.

## The five regressions

| # | Cause | Symptom users report | Signature in the trace | Fix in the demo |
|---|---|---|---|---|
| 1 | A query whose cost grows with the tenant's data (index on `tenant_id` only, then sort) | "Slow for us", from your biggest customers | One `db.query` span that's most of the request, long only for big tenants | Index matching the filter *and* the order: `(tenant_id, created_at DESC)` |
| 2 | N+1 queries | Slow everywhere; worse in production than locally | Dozens of short `db.query` spans in a row | One query with a `JOIN` |
| 3 | Connection pool starvation: reports hold the connections pages need | Slow at peak, or while someone runs reports | Long `db.acquire`, short `db.query` | A separate pool for reports |
| 4 | A slow dependency with no timeout | Slow sometimes, for some requests | One long `http.client` span; database fine | A 100 ms timeout and a fallback |
| 5 | Waiting on row locks held by a batch job | Slow while the job runs; CPU idle | `db.query` long for a trivial `UPDATE`; `pg_stat_activity` shows `wait_event_type = 'Lock'` | The job commits every 100 rows instead of holding all of them |

Measured results: [results.md](results.md).

## Scaling experiments

`scaling/run.sh` (or `make scaling`, about 15 minutes) starts `shopd`, a second small API, as one or more replicas behind nginx, and drives it with an open-model load generator, stepping the rate up until p99 passes 100 ms.

| # | Question | Compared |
|---|---|---|
| 1 | More replicas, or a bigger instance? | 1, 2 and 3 replicas with 1 CPU each, vs 1 replica with 3 CPUs, on ~1 ms of CPU per request |
| 2 | Where should a costly read be cached? | No cache vs an in-process map vs Redis vs an nginx micro-cache, all with a 1 s TTL |
| 3 | "Too many connections": bigger database or pooler? | 4 and 6 app instances × pool of 20, against `max_connections = 100`, directly and through PgBouncer in transaction mode |
| 4 | One client floods the API | No limit vs a per-client token bucket |

## Fan-out and the tail

`make fanout` (runs in Docker, no database needed; `go run ./cmd/fanout` also works if you have Go on the host) simulates one request that waits for 1 to 1,000 servers, each usually fast but slow 1% of the time, and compares waiting for all of them with hedged requests and partial results. It's a simulation with the latency model stated in [`internal/fanout`](internal/fanout/fanout.go), not a measurement; its test checks the simulated tail against 1 - 0.99^N.

## Layout

```
cmd/slowdemo/      seeds the data, runs every scenario, prints Markdown tables
internal/lab/      the API (four routes), the five scenarios, the load driver, the trace summary, and the test
internal/obs/      OpenTelemetry setup, a pgx tracer (query and connection-wait spans), Server-Timing
cmd/shopd/         the API the scaling experiments run (CPU-bound route, cached aggregate, per-client rate limit)
cmd/loadgen/       open-model load generator: steps the rate up and reports the highest one it sustained
cmd/pooldemo/      many app pools against max_connections, directly and through PgBouncer
cmd/fanout/, internal/fanout/  the fan-out tail-latency simulation and its test
scaling/           run.sh, nginx and PgBouncer configs, results.md
compose.yaml       PostgreSQL 17, Jaeger 2, a Go container; profile "scaling" adds the app, nginx, Redis and PgBouncer
```

## Notes

- Everything runs on one machine, so a database round trip is a fraction of a millisecond. N+1 costs one round trip per row: on a network with a 1 ms round trip, the 51-query page in scenario 2 would spend about 50 ms more.
- Instrumentation is hand-written (a pgx `QueryTracer` and `AcquireTracer`, a span around the HTTP call) to keep the dependency list short and the spans easy to read. In an application, the OpenTelemetry contrib instrumentation for your HTTP server, client and database driver does the same.
- The load generator measures server-side time from the root span. What a user feels also includes the network, TLS, the browser's work and the frontend, none of which this lab measures.

## Licence

MIT
