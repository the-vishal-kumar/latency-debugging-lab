.PHONY: up down demo scaling test
up:     ## start PostgreSQL and Jaeger (traces UI: http://localhost:16686)
	docker compose up -d --wait postgres jaeger
down:   ## stop and remove everything
	docker compose down -v
demo: up ## make the API slow five ways, before and after each fix (about a minute)
	docker compose run --rm go go run ./cmd/slowdemo
scaling:  ## replicas, caches, a pooler and rate limits, measured (about 15 minutes)
	./scaling/run.sh
test: up ## pin the trace signature of every regression
	docker compose run --rm go go test -race -count=1 ./...
