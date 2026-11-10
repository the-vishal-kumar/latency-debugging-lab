// Package obs is the observability the demo app carries: OpenTelemetry spans
// for every HTTP request, every database query, every wait for a pooled
// connection and every outgoing call, plus a Server-Timing header so the
// same numbers show up in the browser's DevTools.
//
// Spans go to an in-memory collector (so the demo can summarise them) and,
// if OTEL_EXPORTER_OTLP_ENDPOINT is set, to Jaeger or any OTLP backend.
package obs

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Collector keeps finished spans in memory until Take is called.
type Collector struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (c *Collector) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (c *Collector) OnEnd(s sdktrace.ReadOnlySpan) {
	c.mu.Lock()
	c.spans = append(c.spans, s)
	c.mu.Unlock()
}
func (c *Collector) Shutdown(context.Context) error   { return nil }
func (c *Collector) ForceFlush(context.Context) error { return nil }

// Take returns the spans finished since the last Take, and forgets them.
func (c *Collector) Take() []sdktrace.ReadOnlySpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.spans
	c.spans = nil
	return s
}

// Setup installs a tracer provider that records every span.
func Setup(ctx context.Context, service string) (*Collector, func(), error) {
	col := &Collector{}
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(col),
		sdktrace.WithResource(resource.NewSchemaless(semconv.ServiceName(service))),
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	return col, func() { _ = tp.Shutdown(context.Background()) }, nil
}

func tracer() trace.Tracer { return otel.Tracer("latency-debugging-lab") }

// ---- per-request timing, for the Server-Timing header -----------------------

type timingKey struct{}

type timing struct{ db, dbWait, dep atomic.Int64 } // nanoseconds

func timingFrom(ctx context.Context) *timing {
	t, _ := ctx.Value(timingKey{}).(*timing)
	return t
}

// ---- database: one span per query, one per wait for a connection -------------

// PGTracer implements pgx.QueryTracer and pgxpool.AcquireTracer.
type PGTracer struct{}

type startKey struct{}

func (PGTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	ctx, _ = tracer().Start(ctx, "db.query", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("db.system", "postgresql"), attribute.String("db.query.text", d.SQL)))
	return context.WithValue(ctx, startKey{}, time.Now())
}

func (PGTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	s := trace.SpanFromContext(ctx)
	if d.Err != nil {
		s.RecordError(d.Err)
		s.SetStatus(codes.Error, d.Err.Error())
	}
	s.End()
	if t, st := timingFrom(ctx), ctx.Value(startKey{}); t != nil && st != nil {
		t.db.Add(int64(time.Since(st.(time.Time))))
	}
}

func (PGTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	ctx, _ = tracer().Start(ctx, "db.acquire")
	return context.WithValue(ctx, startKey{}, time.Now())
}

func (PGTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, d pgxpool.TraceAcquireEndData) {
	s := trace.SpanFromContext(ctx)
	if d.Err != nil {
		s.RecordError(d.Err)
	}
	s.End()
	if t, st := timingFrom(ctx), ctx.Value(startKey{}); t != nil && st != nil {
		t.dbWait.Add(int64(time.Since(st.(time.Time))))
	}
}

// Pool opens a traced pool with at most maxConns connections.
func Pool(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = maxConns
	cfg.ConnConfig.Tracer = PGTracer{}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// ---- outgoing calls ------------------------------------------------------------

// Call wraps an outgoing call to another service in a span.
func Call(ctx context.Context, name string, fn func(ctx context.Context) error) error {
	ctx, s := tracer().Start(ctx, "http.client "+name, trace.WithSpanKind(trace.SpanKindClient))
	start := time.Now()
	err := fn(ctx)
	if err != nil {
		s.RecordError(err)
		s.SetStatus(codes.Error, err.Error())
	}
	s.End()
	if t := timingFrom(ctx); t != nil {
		t.dep.Add(int64(time.Since(start)))
	}
	return err
}

// ---- incoming requests -----------------------------------------------------------

// Handler is an endpoint that returns its JSON body, so the middleware can
// set Server-Timing before anything is written.
type Handler func(ctx context.Context, r *http.Request) ([]byte, error)

// Serve wraps h in a server span and adds Server-Timing:
//
//	Server-Timing: db;dur=12.3, db-wait;dur=0.1, dep;dur=20.4, total;dur=35.0
func Serve(route string, h Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		t := &timing{}
		ctx := context.WithValue(r.Context(), timingKey{}, t)
		ctx, span := tracer().Start(ctx, "GET "+route, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.route", route)))
		defer span.End()
		body, err := h(ctx, r)
		ms := func(ns int64) float64 { return float64(ns) / 1e6 }
		w.Header().Set("Server-Timing", fmt.Sprintf("db;dur=%.1f, db-wait;dur=%.1f, dep;dur=%.1f, total;dur=%.1f",
			ms(t.db.Load()), ms(t.dbWait.Load()), ms(t.dep.Load()), ms(int64(time.Since(start)))))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
}
