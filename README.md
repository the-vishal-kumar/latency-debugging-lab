# latency-debugging-lab

A small multi-tenant API, made slow in five common ways. For each one, the demo drives load before and after the fix and summarises what the traces show, so you can learn each cause's **signature**: the shape it leaves in a trace before you know what it is.

Companion to the post *"A user says a page is lagging: how do you debug it end to end?"*

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

## Layout

```
cmd/slowdemo/      seeds the data, runs every scenario, prints Markdown tables
internal/lab/      the API (four routes), the five scenarios, the load driver, the trace summary, and the test
internal/obs/      OpenTelemetry setup, a pgx tracer (query and connection-wait spans), Server-Timing
compose.yaml       PostgreSQL 17, Jaeger 2, and a Go container
```

## Notes

- Everything runs on one machine, so a database round trip is a fraction of a millisecond. N+1 costs one round trip per row: on a network with a 1 ms round trip, the 51-query page in scenario 2 would spend about 50 ms more.
- Instrumentation is hand-written (a pgx `QueryTracer` and `AcquireTracer`, a span around the HTTP call) to keep the dependency list short and the spans easy to read. In an application, the OpenTelemetry contrib instrumentation for your HTTP server, client and database driver does the same.
- The load generator measures server-side time from the root span. What a user feels also includes the network, TLS, the browser's work and the frontend; the post covers those layers.

## Licence

MIT
