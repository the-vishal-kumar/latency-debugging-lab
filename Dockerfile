# shopd, the API the scaling experiments run as one or several replicas.
FROM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /shopd ./cmd/shopd

FROM scratch
COPY --from=build /shopd /shopd
USER 65534
ENTRYPOINT ["/shopd"]
