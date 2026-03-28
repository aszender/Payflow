package telemetry

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type TraceConfig struct {
	Enabled      bool
	ServiceName  string
	OTLPEndpoint string
	OTLPInsecure bool
}

func SetupTracing(ctx context.Context, cfg TraceConfig) (func(context.Context) error, error) {
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}

	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = "payflow"
	}

	tpOptions := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", serviceName),
		)),
	}

	if endpoint := strings.TrimSpace(cfg.OTLPEndpoint); endpoint != "" {
		exporterOptions := []otlptracehttp.Option{}
		if strings.Contains(endpoint, "://") {
			exporterOptions = append(exporterOptions, otlptracehttp.WithEndpointURL(endpoint))
		} else {
			exporterOptions = append(exporterOptions, otlptracehttp.WithEndpoint(endpoint))
		}
		if cfg.OTLPInsecure {
			exporterOptions = append(exporterOptions, otlptracehttp.WithInsecure())
		}

		exporter, err := otlptracehttp.New(ctx, exporterOptions...)
		if err != nil {
			return nil, err
		}
		tpOptions = append(tpOptions, sdktrace.WithBatcher(exporter))
	}

	provider := sdktrace.NewTracerProvider(tpOptions...)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return provider.Shutdown, nil
}
