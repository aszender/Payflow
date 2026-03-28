package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTracingMiddleware_CreatesServerSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithResource(resource.Empty()),
	)

	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		_ = provider.Shutdown(t.Context())
	})

	r := chi.NewRouter()
	r.Use(RequestID)
	r.Use(Tracing("payflow-test"))
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if got := span.Name; got != "GET /health" {
		t.Fatalf("expected span name GET /health, got %q", got)
	}

	attrs := make(map[string]string)
	for _, attr := range span.Attributes {
		attrs[string(attr.Key)] = attr.Value.Emit()
	}

	if got := attrs["http.method"]; got != "GET" {
		t.Fatalf("expected http.method GET, got %q", got)
	}
	if got := attrs["http.route"]; got != "/health" {
		t.Fatalf("expected http.route /health, got %q", got)
	}
	if got := attrs["http.status_code"]; got != "200" {
		t.Fatalf("expected http.status_code 200, got %q", got)
	}
	if attrs["request.id"] == "" {
		t.Fatal("expected request.id attribute to be set")
	}
}
