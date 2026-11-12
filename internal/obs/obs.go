// Package obs is the observability the demo app carries: OpenTelemetry spans
// for every HTTP request, every database query, every wait for a pooled
// connection and every outgoing call, plus a Server-Timing header so the
// same numbers show up in the browser's DevTools.
//
// Spans go to an in-memory collector (so the demo can summarise them) and,
// if OTEL_EXPORTER_OTLP_ENDPOINT is set, to Jaeger or any OTLP backend.
//
// Attributes follow OpenTelemetry semantic conventions v1.43 (the version
// the SDK itself uses). Span names for database and dependency spans are
// kept short and fixed (db.query, db.acquire, http.client <name>) so a
// reader can search for them in Jaeger; see the README for why.
package obs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
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

// Resource describes the process emitting spans: service.name and
// service.version, the SDK, host and runtime, then anything in
// OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES (which win).
func Resource(ctx context.Context, service string) (*resource.Resource, error) {
	version := "dev"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	return resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(semconv.ServiceName(service), semconv.ServiceVersion(version)),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithProcessRuntimeName(),
		resource.WithProcessRuntimeVersion(),
		resource.WithProcessPID(),
		resource.WithFromEnv(),
	)
}

// Sampler is parent-based: a request that arrives with a sampled
// traceparent stays sampled, and a root span is always sampled, because the
// demo's tables are computed from every span. Set OTEL_TRACES_SAMPLER (for
// example parentbased_traceidratio with OTEL_TRACES_SAMPLER_ARG=0.1) to
// sample as a production service would; the tables then cover only the
// sampled requests.
func samplerOption() []sdktrace.TracerProviderOption {
	if os.Getenv("OTEL_TRACES_SAMPLER") != "" {
		return nil // the SDK reads OTEL_TRACES_SAMPLER and OTEL_TRACES_SAMPLER_ARG itself
	}
	return []sdktrace.TracerProviderOption{sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample()))}
}

// Setup installs a tracer provider and W3C trace-context propagation. The
// returned stop function flushes buffered spans, waiting at most 5 s.
func Setup(ctx context.Context, service string) (*Collector, func(), error) {
	res, err := Resource(ctx, service)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, nil, fmt.Errorf("resource: %w", err)
	}
	col := &Collector{}
	opts := append(samplerOption(),
		sdktrace.WithSpanProcessor(col),
		sdktrace.WithResource(res),
	)
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("otlp exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			log.Printf("obs: flushing spans: %v", err)
		}
	}
	return col, stop, nil
}

func tracer() trace.Tracer {
	return otel.Tracer("github.com/the-vishal-kumar/latency-debugging-lab/internal/obs")
}

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

// operation is the first SQL keyword (SELECT, UPDATE, ...), for
// db.operation.name. Good enough for the statements this repo runs.
func operation(sql string) string {
	f := strings.Fields(sql)
	if len(f) == 0 {
		return ""
	}
	return strings.ToUpper(f[0])
}

func (PGTracer) TraceQueryStart(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	attrs := []attribute.KeyValue{semconv.DBSystemNamePostgreSQL, semconv.DBQueryText(d.SQL)}
	if op := operation(d.SQL); op != "" {
		attrs = append(attrs, semconv.DBOperationName(op))
	}
	if c != nil {
		cfg := c.Config()
		attrs = append(attrs, semconv.DBNamespace(cfg.Database), semconv.ServerAddress(cfg.Host), semconv.ServerPort(int(cfg.Port)))
	}
	ctx, _ = tracer().Start(ctx, "db.query", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	return context.WithValue(ctx, startKey{}, time.Now())
}

func (PGTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	s := trace.SpanFromContext(ctx)
	if d.Err != nil {
		s.RecordError(d.Err)
		s.SetStatus(codes.Error, d.Err.Error())
		s.SetAttributes(semconv.ErrorType(d.Err))
	}
	s.End()
	if t, st := timingFrom(ctx), ctx.Value(startKey{}); t != nil && st != nil {
		t.db.Add(int64(time.Since(st.(time.Time))))
	}
}

func (PGTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	ctx, _ = tracer().Start(ctx, "db.acquire", trace.WithAttributes(semconv.DBSystemNamePostgreSQL))
	return context.WithValue(ctx, startKey{}, time.Now())
}

func (PGTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, d pgxpool.TraceAcquireEndData) {
	s := trace.SpanFromContext(ctx)
	if d.Err != nil {
		s.RecordError(d.Err)
		s.SetStatus(codes.Error, d.Err.Error())
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

// Call wraps an outgoing call to another service in a client span. fn
// receives a context carrying the span; pass it to Inject so the other
// service joins the same trace.
func Call(ctx context.Context, name string, fn func(ctx context.Context) error) error {
	ctx, s := tracer().Start(ctx, "http.client "+name, trace.WithSpanKind(trace.SpanKindClient))
	start := time.Now()
	err := fn(ctx)
	if err != nil {
		s.RecordError(err)
		s.SetStatus(codes.Error, err.Error())
		s.SetAttributes(semconv.ErrorType(err))
	}
	s.End()
	if t := timingFrom(ctx); t != nil {
		t.dep.Add(int64(time.Since(start)))
	}
	return err
}

// Inject adds the traceparent (and baggage) header for the span in ctx, and
// records the request's method and URL on that span.
func Inject(ctx context.Context, req *http.Request) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	trace.SpanFromContext(ctx).SetAttributes(
		semconv.HTTPRequestMethodKey.String(req.Method),
		semconv.URLFull(req.URL.Redacted()),
		semconv.ServerAddress(req.URL.Hostname()),
	)
}

// ---- incoming requests -----------------------------------------------------------

// Handler is an endpoint that returns its JSON body, so the middleware can
// set Server-Timing before anything is written.
type Handler func(ctx context.Context, r *http.Request) ([]byte, error)

// StatusError is an error with the HTTP status the client should see.
// Errors of any other type become 500.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return e.Msg }

// BadRequest is a 400 with msg as the body.
func BadRequest(msg string) error { return &StatusError{http.StatusBadRequest, msg} }

// Serve wraps h in a server span (continuing the caller's trace if the
// request carries a traceparent) and adds Server-Timing:
//
//	Server-Timing: db;dur=12.3, db-wait;dur=0.1, dep;dur=20.4, total;dur=35.0
func Serve(route string, h Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		t := &timing{}
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx = context.WithValue(ctx, timingKey{}, t)
		ctx, span := tracer().Start(ctx, r.Method+" "+route, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPRoute(route),
				semconv.HTTPRequestMethodKey.String(r.Method),
				semconv.URLPath(r.URL.Path),
				semconv.URLScheme("http"),
				semconv.UserAgentOriginal(r.UserAgent()),
			))
		defer span.End()
		body, err := h(ctx, r)
		ms := func(ns int64) float64 { return float64(ns) / 1e6 }
		w.Header().Set("Server-Timing", fmt.Sprintf("db;dur=%.1f, db-wait;dur=%.1f, dep;dur=%.1f, total;dur=%.1f",
			ms(t.db.Load()), ms(t.dbWait.Load()), ms(t.dep.Load()), ms(int64(time.Since(start)))))
		if err != nil {
			code, msg := http.StatusInternalServerError, "internal error"
			var se *StatusError
			if errors.As(err, &se) {
				code, msg = se.Code, se.Msg
			}
			span.SetAttributes(semconv.HTTPResponseStatusCode(code))
			if code >= 500 { // semconv: 4xx is the client's error, not the server span's
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				span.SetAttributes(semconv.ErrorType(err))
			}
			http.Error(w, msg, code)
			return
		}
		span.SetAttributes(semconv.HTTPResponseStatusCode(http.StatusOK))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
}
