# shopd, the API the scaling experiments run as one or several replicas.
# Multi-stage: a full Go toolchain builds a static binary; the final image
# is scratch (no shell, no package manager), running as an unprivileged user.
FROM golang:1.27.4-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /shopd ./cmd/shopd

FROM scratch
LABEL org.opencontainers.image.source="https://github.com/the-vishal-kumar/latency-debugging-lab" \
      org.opencontainers.image.description="shopd: the API the scaling experiments run" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /shopd /shopd
USER 65534:65534
EXPOSE 8080
HEALTHCHECK --interval=2s --timeout=3s --start-period=5s --retries=15 CMD ["/shopd", "-healthcheck"]
ENTRYPOINT ["/shopd"]
