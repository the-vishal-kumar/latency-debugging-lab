# Scaling results

Measured on 2026-11-10 with `scaling/run.sh` on one laptop (Intel i7-8550U, 4 cores / 8 threads). The load generator, nginx, the replicas and PostgreSQL share those cores, so absolute numbers are this machine's; the comparisons between rows are the point.

"Sustained" = the highest rate tried at which p99 stayed under 100 ms, under 1% errors, and at least 95% of requests were served. Each rate ran for 8 s, open model (requests keep arriving whether or not earlier ones finished).

## 1. More replicas or a bigger instance? (`/cpu`: about 1 ms of CPU per request)

| Setup | Sustained req/s | p50 there | p99 there | Next rate tried |
|---|---:|---:|---:|---|
| 1 replica, 1 CPU | 1000 | 3.0 ms | 26 ms | 1250 req/s: p99 2012 ms, 511 errors |
| 2 replicas, 1 CPU each | 1500 | 1.5 ms | 24 ms | 1750 req/s: p99 445 ms, 0 errors |
| 3 replicas, 1 CPU each | 1500 | 1.8 ms | 27 ms | 1750 req/s: p99 136 ms, 0 errors |
| 1 replica, 3 CPUs | 2000 | 4.0 ms | 45 ms | 2250 req/s: p99 435 ms, 0 errors |

## 2. Caching a costly read (`/stats`: a 20 ms aggregate over 100,000 rows; 10 tenants; 1 replica, 1 CPU)

| Cache (1 s TTL) | Sustained req/s | p50 there | p99 there | Next rate tried |
|---|---:|---:|---:|---|
| none | 100 | 43 ms | 66 ms | 200 req/s: p99 4242 ms, 0 errors |
| in-process map in each replica | 800 | 0.5 ms | 78 ms | 1600 req/s: p99 4861 ms, 2660 errors |
| Redis | 800 | 0.8 ms | 69 ms | 1600 req/s: p99 4919 ms, 3363 errors |
| nginx micro-cache in front (app does no caching) | 6400 | 0.3 ms | 1.3 ms | — |

## 3. "Too many connections": a bigger database, or a pooler?

PostgreSQL max_connections = 100; each instance has a pool of 20 and runs 20 concurrent queries that each need ~5 ms of database CPU.

| Path | Instances | Client connections wanted | Server connections used (peak) | Queries/s | p50 | p99 | Errors |
|---|---:|---:|---:|---:|---:|---:|---|
| direct to PostgreSQL | 4 | 80 | 90 | 968 | 84.0 ms | 198.8 ms | 0 |
| direct to PostgreSQL | 6 | 120 | 99 | 803 | 112.7 ms | 234.2 ms | 1003 × `53300 sorry, too many clients already` |
| through PgBouncer (transaction mode, pool 20) | 4 | 80 | 30 | 792 | 99.4 ms | 148.5 ms | 0 |
| through PgBouncer (transaction mode, pool 20) | 6 | 120 | 31 | 793 | 151.9 ms | 196.9 ms | 0 |

## 4. One noisy client (`/cpu`; 10 normal clients at 300 req/s in total, plus one client at 1,500 req/s; 1 replica, 1 CPU)

| Per-client limit | Normal clients: served/s | p50 | p99 | Normal clients: errors + 429s | Noisy client: served/s | Noisy: 429s | Noisy: errors |
|---|---:|---:|---:|---:|---:|---:|---:|
| none | 200 | 2623 ms | 4963 ms | 803 | 1016 | 0 | 3872 |
| 50 req/s per client (token bucket in the app) | 300 | 1.1 ms | 10 ms | 0 | 56 | 11551 | 0 |
