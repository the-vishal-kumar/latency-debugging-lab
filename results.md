# Results

Measured on 2026-11-10 with `make demo` on a laptop (Intel i7-8550U, 8 threads, 10 GB RAM): the app, PostgreSQL and the load generator all on one machine, so a database round trip is about 0.1 to 0.3 ms. Across a network, every number that depends on round trips (N+1 most of all) is larger.

A second run gave the same picture, though the spread was not uniform (the two runs differ by about 5% for the starved pool and the lock wait, about 25% for the big tenant, and 45% (5 ms) for N+1, whose absolute times are small): big-tenant p50 829 vs 1035 ms, N+1 p50 11 vs 16 ms, starved-pool p50 291 vs 303 ms, no-timeout p99 2002 ms both times, lock-wait p99 558 vs 529 ms. The run below is the second one.

PostgreSQL 17.11. Small tenant: 2000 orders; big tenant: 1000000 orders; 1,000 customers each. 3s of load per variant.

### 1. A query that grows with the tenant (slow only for big customers)

| Variant | Route | Requests | p50 | p99 | Queries/req | DB time/req | Waiting for a connection/req | Dependency/req |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| index on (tenant_id) only | `/orders/big` | 15 | 829 ms | 1644 ms | 1 | 892 ms | 2.46 ms | 0.00 ms |
| index on (tenant_id) only | `/orders/small` | 1002 | 11 ms | 24 ms | 1 | 12 ms | 0.04 ms | 0.00 ms |
| index on (tenant_id, created_at DESC) | `/orders/big` | 8004 | 1.13 ms | 2.94 ms | 1 | 1.14 ms | 0.00 ms | 0.00 ms |
| index on (tenant_id, created_at DESC) | `/orders/small` | 8203 | 1.09 ms | 2.91 ms | 1 | 1.10 ms | 0.00 ms | 0.00 ms |


### 2. N+1 queries (slow everywhere, worse the further the database is)

| Variant | Route | Requests | p50 | p99 | Queries/req | DB time/req | Waiting for a connection/req | Dependency/req |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| orders, then one query per order for the customer | `/orders/small` | 2041 | 11 ms | 21 ms | 51 | 11 ms | 0.17 ms | 0.00 ms |
| one query with a JOIN | `/orders/small` | 15690 | 1.09 ms | 3.30 ms | 1 | 1.09 ms | 0.00 ms | 0.00 ms |


### 3. Connection pool starvation (slow at peak or while reports run; queries themselves fast)

| Variant | Route | Requests | p50 | p99 | Queries/req | DB time/req | Waiting for a connection/req | Dependency/req |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| one pool of 5 for pages and reports | `/orders/small` | 293 | 291 ms | 306 ms | 1 | 0.50 ms | 166 ms | 0.00 ms |
| one pool of 5 for pages and reports | `/report` | 51 | 309 ms | 607 ms | 1 | 301 ms | 64 ms | 0.00 ms |
| pages: pool of 5; reports: their own pool of 2 | `/orders/small` | 18136 | 2.30 ms | 4.50 ms | 1 | 0.71 ms | 1.62 ms | 0.00 ms |
| pages: pool of 5; reports: their own pool of 2 | `/report` | 24 | 902 ms | 913 ms | 1 | 301 ms | 528 ms | 0.00 ms |


### 4. A slow dependency with no timeout (slow sometimes, for some requests, with nothing wrong in our database)

| Variant | Route | Requests | p50 | p99 | Queries/req | DB time/req | Waiting for a connection/req | Dependency/req |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| no timeout | `/orders/small` | 284 | 22 ms | 2002 ms | 1 | 0.64 ms | 0.26 ms | 118 ms |
| 100 ms timeout, page renders without recommendations | `/orders/small` | 954 | 21 ms | 101 ms | 1 | 0.48 ms | 0.00 ms | 25 ms |


### 5. Waiting on a lock held by a batch job (slow while the job runs; CPU idle)

| Variant | Route | Requests | p50 | p99 | Queries/req | DB time/req | Waiting for a connection/req | Dependency/req |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| job updates all rows in one 500 ms transaction | `/touch` | 135 | 5.12 ms | 558 ms | 1 | 190 ms | 0.77 ms | 0.00 ms |
| job updates 100 rows per short transaction | `/touch` | 4947 | 4.58 ms | 7.45 ms | 1 | 4.65 ms | 0.00 ms | 0.00 ms |

- job updates all rows in one 500 ms transaction: pg_stat_activity showed sessions waiting on a lock in 153 of 158 samples
- job updates 100 rows per short transaction: pg_stat_activity showed sessions waiting on a lock in 6 of 149 samples

