# Everything runs in containers: Docker is the only requirement.
GO_IMAGE   ?= golang:1.27.4-bookworm
STATICCHECK = honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOVULNCHECK = golang.org/x/vuln/cmd/govulncheck@v1.9.0
# One-off Go container as the calling user, so no root-owned files appear.
GO = docker run --rm --user $$(id -u):$$(id -g) -e HOME=/tmp -e GOCACHE=/tmp/.cache/go-build \
     -e GOPATH=/tmp/go -v "$$PWD":/src -w /src $(GO_IMAGE)

.PHONY: help up down demo test unit lint vuln fanout scaling image
.DEFAULT_GOAL := help

help:     ## list the targets
	@grep -E '^[a-z]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  make %-8s %s\n", $$1, $$2}'
up:       ## start PostgreSQL and Jaeger (traces UI: http://localhost:16686)
	docker compose up -d --wait postgres jaeger
down:     ## stop and remove everything, volumes included
	docker compose --profile scaling --profile tools down -v
demo: up  ## make the API slow five ways, before and after each fix (about a minute)
	docker compose run --rm go go run ./cmd/slowdemo
test: up  ## every test, -race, including each regression's trace signature (needs PostgreSQL)
	docker compose run --rm go go test -race -count=1 ./...
unit:     ## the tests that need no database (fan-out, obs, shopd)
	$(GO) go test -race -count=1 ./...
lint:     ## gofmt, go vet, staticcheck
	$(GO) sh -c 'test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }; go vet ./... && go run $(STATICCHECK) ./...'
vuln:     ## govulncheck against the Go vulnerability database
	$(GO) go run $(GOVULNCHECK) ./...
fanout:   ## the fan-out and hedging simulation (no database)
	$(GO) go run ./cmd/fanout
scaling:  ## replicas, caches, a pooler and rate limits, measured (about 15 minutes)
	./scaling/run.sh
image:    ## build the shopd image
	docker build -t latency-debugging-lab-app .
