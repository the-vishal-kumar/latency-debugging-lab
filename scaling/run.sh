#!/usr/bin/env bash
# Runs the four scaling experiments and writes scaling/results.md.
# Takes about 15 minutes. Needs Docker; nothing else.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p scaling/raw
OUT=scaling/results.md
DC="docker compose --profile scaling"
LOG=scaling/raw/compose.log
lg() { docker compose run --rm -T go loadgen "$@" 2>>scaling/raw/loadgen.log; }
# Compose output goes to a log; on failure, show its tail instead of exiting silently.
quiet() { "$@" >>"$LOG" 2>&1 || { echo "failed: $*" >&2; tail -20 "$LOG" >&2; exit 1; }; }
: > "$LOG"

echo "== starting PostgreSQL and seeding 1,000,000 orders over 10 tenants"
quiet docker compose up -d --wait postgres
docker compose exec -T postgres psql -q -U lab -d lab >/dev/null <<'SQL'
DROP SCHEMA IF EXISTS scale CASCADE;
CREATE SCHEMA scale;
CREATE TABLE scale.orders (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, tenant_id int NOT NULL, total numeric(10,2) NOT NULL);
INSERT INTO scale.orders (tenant_id, total) SELECT 1 + (g % 10), round((random()*500)::numeric, 2) FROM generate_series(1, 1000000) g;
CREATE INDEX ON scale.orders (tenant_id);
VACUUM ANALYZE scale.orders;
SQL
# Build into the gobin volume (on PATH in the go container), not the source tree.
quiet docker compose run --rm -T go go build -o /go/bin/ ./cmd/loadgen ./cmd/pooldemo
: > scaling/raw/loadgen.log

# start N replicas with the given settings, and an nginx that knows all of them
deploy() { # replicas, then env assignments
  local n=$1; shift
  quiet env "$@" $DC up -d --build --wait --force-recreate --scale app="$n" app nginx redis
  sleep 2
}

{
  echo "# Scaling results"
  echo
  echo "Measured on $(date -u +%F) with \`scaling/run.sh\` on one laptop (Intel i7-8550U, 4 cores / 8 threads). The load generator, nginx, the replicas and PostgreSQL share those cores, so absolute numbers are this machine's; the comparisons between rows are the point."
  echo
  echo "\"Sustained\" = the highest rate tried at which p99 stayed under 100 ms, under 1% errors, and at least 95% of requests were served. Each rate ran for 8 s, open model (requests keep arriving whether or not earlier ones finished)."
  echo
} > "$OUT"

echo "== 1. replicas vs a bigger instance (/cpu, ~1 ms of CPU per request)"
{
  echo "## 1. More replicas or a bigger instance? (\`/cpu\`: about 1 ms of CPU per request)"
  echo
  echo "| Setup | Sustained req/s | p50 there | p99 there | Next rate tried |"
  echo "|---|---:|---:|---:|---|"
} >> "$OUT"
R=500,750,1000,1250,1500,1750,2000,2250,2500,2750,3000,3500
deploy 1 APP_CPUS=1; lg -url http://nginx/cpu -rates $R -label "1 replica, 1 CPU" >> "$OUT"
deploy 2 APP_CPUS=1; lg -url http://nginx/cpu -rates $R -label "2 replicas, 1 CPU each" >> "$OUT"
deploy 3 APP_CPUS=1; lg -url http://nginx/cpu -rates $R -label "3 replicas, 1 CPU each" >> "$OUT"
deploy 1 APP_CPUS=3; lg -url http://nginx/cpu -rates $R -label "1 replica, 3 CPUs" >> "$OUT"

echo "== 2. caching a costly read (/stats)"
{
  echo
  echo "## 2. Caching a costly read (\`/stats\`: a 20 ms aggregate over 100,000 rows; 10 tenants; 1 replica, 1 CPU)"
  echo
  echo "| Cache (1 s TTL) | Sustained req/s | p50 there | p99 there | Next rate tried |"
  echo "|---|---:|---:|---:|---|"
} >> "$OUT"
R=50,100,200,400,800,1600,3200,6400
deploy 1 APP_CPUS=1 CACHE=none;  lg -url http://nginx/stats -query tenant -rates $R -label "none" >> "$OUT"
deploy 1 APP_CPUS=1 CACHE=local; lg -url http://nginx/stats -query tenant -rates $R -label "in-process map in each replica" >> "$OUT"
deploy 1 APP_CPUS=1 CACHE=redis; lg -url http://nginx/stats -query tenant -rates $R -label "Redis" >> "$OUT"
deploy 1 APP_CPUS=1 CACHE=none NGINX_MODE=cache; lg -url http://nginx/stats -query tenant -rates $R -label "nginx micro-cache in front (app does no caching)" >> "$OUT"

echo "== 3. too many connections"
# The PgBouncer image has no healthcheck, so --wait returns once it starts.
quiet $DC up -d --wait pgbouncer; sleep 3
{
  echo
  echo "## 3. \"Too many connections\": a bigger database, or a pooler?"
  echo
  docker compose run --rm -T go pooldemo \
    -direct "postgres://lab:lab@postgres:5432/lab?sslmode=disable" \
    -bouncer "postgres://lab:lab@pgbouncer:6432/lab?sslmode=disable" -instances 4,6
} >> "$OUT"

echo "== 4. one noisy client"
{
  echo
  echo "## 4. One noisy client (\`/cpu\`; 10 normal clients at 300 req/s in total, plus one client at 1,500 req/s; 1 replica, 1 CPU)"
  echo
  echo "| Per-client limit | Normal clients: served/s | p50 | p99 | Normal clients: errors + 429s | Noisy client: served/s | Noisy: 429s | Noisy: errors |"
  echo "|---|---:|---:|---:|---:|---:|---:|---:|"
} >> "$OUT"
deploy 1 APP_CPUS=1 RATE_LIMIT=0;  lg -url http://nginx/cpu -rates 300 -noisy-rate 1500 -label "none" >> "$OUT"
deploy 1 APP_CPUS=1 RATE_LIMIT=50; lg -url http://nginx/cpu -rates 300 -noisy-rate 1500 -label "50 req/s per client (token bucket in the app)" >> "$OUT"

quiet $DC stop app nginx redis pgbouncer
echo "== done: $OUT"
