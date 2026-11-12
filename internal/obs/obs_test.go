package obs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func setup(t *testing.T) *Collector {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_TRACES_SAMPLER", "")
	col, stop, err := Setup(context.Background(), "obs-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return col
}

func attr(s sdktrace.ReadOnlySpan, k string) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == k {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func only(t *testing.T, spans []sdktrace.ReadOnlySpan, kind trace.SpanKind) sdktrace.ReadOnlySpan {
	t.Helper()
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.SpanKind() == kind {
			out = append(out, s)
		}
	}
	if len(out) != 1 {
		t.Fatalf("want one %v span, got %d", kind, len(out))
	}
	return out[0]
}

func TestServeSpanAndServerTiming(t *testing.T) {
	col := setup(t)
	h := Serve("/x", func(ctx context.Context, _ *http.Request) ([]byte, error) {
		_ = Call(ctx, "dep", func(context.Context) error { time.Sleep(2 * time.Millisecond); return nil })
		return []byte(`{}`), nil
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	st := rec.Header().Get("Server-Timing")
	if !regexp.MustCompile(`^db;dur=[\d.]+, db-wait;dur=[\d.]+, dep;dur=([\d.]+), total;dur=[\d.]+$`).MatchString(st) {
		t.Fatalf("Server-Timing = %q", st)
	}
	if regexp.MustCompile(`dep;dur=0\.[01],`).MatchString(st) {
		t.Errorf("dependency time not counted: %q", st)
	}
	spans := col.Take()
	srv := only(t, spans, trace.SpanKindServer)
	if srv.Name() != "GET /x" {
		t.Errorf("server span name = %q", srv.Name())
	}
	for k, want := range map[string]string{"http.route": "/x", "http.request.method": "GET", "url.path": "/x"} {
		if v, ok := attr(srv, k); !ok || v.AsString() != want {
			t.Errorf("%s = %q, want %q", k, v.AsString(), want)
		}
	}
	if v, _ := attr(srv, "http.response.status_code"); v.AsInt64() != 200 {
		t.Errorf("status code attribute = %d", v.AsInt64())
	}
	cli := only(t, spans, trace.SpanKindClient)
	if cli.Parent().SpanID() != srv.SpanContext().SpanID() {
		t.Error("dependency span is not a child of the server span")
	}
	if v, _ := attr(srv, "service.name"); v.AsString() != "" {
		t.Error("service.name belongs on the resource, not the span")
	}
	if got := srv.Resource().Set(); !got.HasValue("service.name") || !got.HasValue("service.version") || !got.HasValue("telemetry.sdk.version") {
		t.Errorf("resource is missing service or SDK attributes: %v", srv.Resource())
	}
}

func TestServeContinuesIncomingTrace(t *testing.T) {
	col := setup(t)
	h := Serve("/x", func(context.Context, *http.Request) ([]byte, error) { return nil, nil })
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	h.ServeHTTP(httptest.NewRecorder(), req)
	srv := only(t, col.Take(), trace.SpanKindServer)
	if got := srv.SpanContext().TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace id = %s, want the caller's", got)
	}
	if got := srv.Parent().SpanID().String(); got != "00f067aa0ba902b7" {
		t.Errorf("parent span = %s, want the caller's", got)
	}
}

func TestInjectPropagatesToDependency(t *testing.T) {
	col := setup(t)
	var got string
	dep := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Header.Get("traceparent") }))
	defer dep.Close()
	h := Serve("/x", func(ctx context.Context, _ *http.Request) ([]byte, error) {
		return nil, Call(ctx, "dep", func(ctx context.Context) error {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, dep.URL, nil)
			Inject(ctx, req)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
			}
			return err
		})
	})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	cli := only(t, col.Take(), trace.SpanKindClient)
	want := "00-" + cli.SpanContext().TraceID().String() + "-" + cli.SpanContext().SpanID().String() + "-01"
	if got != want {
		t.Errorf("dependency saw traceparent %q, want %q", got, want)
	}
}

func TestServeErrors(t *testing.T) {
	col := setup(t)
	for _, tc := range []struct {
		err      error
		code     int
		body     string
		spanCode codes.Code
	}{
		{BadRequest("customer must be a positive integer"), 400, "customer must be a positive integer\n", codes.Unset},
		{errors.New("pq: secret detail"), 500, "internal error\n", codes.Error},
	} {
		rec := httptest.NewRecorder()
		Serve("/x", func(context.Context, *http.Request) ([]byte, error) { return nil, tc.err }).
			ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if rec.Code != tc.code || rec.Body.String() != tc.body {
			t.Errorf("%v: got %d %q, want %d %q", tc.err, rec.Code, rec.Body.String(), tc.code, tc.body)
		}
		srv := only(t, col.Take(), trace.SpanKindServer)
		if srv.Status().Code != tc.spanCode {
			t.Errorf("%v: span status %v, want %v", tc.err, srv.Status().Code, tc.spanCode)
		}
		if v, _ := attr(srv, "http.response.status_code"); v.AsInt64() != int64(tc.code) {
			t.Errorf("%v: status code attribute %d", tc.err, v.AsInt64())
		}
	}
}

func TestResourceFromEnv(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment.name=ci")
	t.Setenv("OTEL_SERVICE_NAME", "renamed")
	res, err := Resource(context.Background(), "obs-test")
	if err != nil {
		t.Fatal(err)
	}
	set := res.Set()
	if v, _ := set.Value("service.name"); v.AsString() != "renamed" {
		t.Errorf("OTEL_SERVICE_NAME should win: %v", v.AsString())
	}
	if v, _ := set.Value("deployment.environment.name"); v.AsString() != "ci" {
		t.Errorf("OTEL_RESOURCE_ATTRIBUTES not applied: %v", res)
	}
}

func TestOperation(t *testing.T) {
	for in, want := range map[string]string{"SELECT 1": "SELECT", "\n\t update x": "UPDATE", "": ""} {
		if got := operation(in); got != want {
			t.Errorf("operation(%q) = %q, want %q", in, got, want)
		}
	}
}
